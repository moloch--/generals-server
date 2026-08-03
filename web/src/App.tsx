import {DataGrid, type DataGridColumn} from "@heroui-pro/react/data-grid";
import {KPI} from "@heroui-pro/react/kpi";
import type {IconDefinition} from "@fortawesome/fontawesome-svg-core";
import {
  faAddressCard,
  faArrowDown,
  faArrowRightToBracket,
  faArrowUp,
  faArrowsRotate,
  faChevronLeft,
  faChevronRight,
  faCircleCheck,
  faCircleXmark,
  faClock,
  faDatabase,
  faDoorOpen,
  faGamepad,
  faGaugeHigh,
  faKey,
  faLayerGroup,
  faMagnifyingGlass,
  faPlugCircleXmark,
  faRightFromBracket,
  faSatelliteDish,
  faShieldHalved,
  faTowerBroadcast,
  faTrashCan,
  faTriangleExclamation,
  faUserSlash,
  faUsers,
  faUsersSlash,
} from "@fortawesome/free-solid-svg-icons";
import {FontAwesomeIcon} from "@fortawesome/react-fontawesome";
import {Alert} from "@heroui/react/alert";
import {AlertDialog} from "@heroui/react/alert-dialog";
import {Button} from "@heroui/react/button";
import {Card} from "@heroui/react/card";
import {Chip} from "@heroui/react/chip";
import {FieldError} from "@heroui/react/field-error";
import {Input} from "@heroui/react/input";
import {Label} from "@heroui/react/label";
import {Modal} from "@heroui/react/modal";
import {SearchField} from "@heroui/react/search-field";
import {TextField} from "@heroui/react/textfield";
import {FormEvent, useCallback, useEffect, useMemo, useState} from "react";

import {
  AdminApi,
  ApiError,
  type Game,
  type Overview,
  type Profile,
  type ProfilePage,
  type Session,
  type SnapshotEvent,
} from "./api";

const tokenStorageKey = "generals-server-admin-token";
const profilePageSize = 25;
const appIconURL = `${import.meta.env.BASE_URL}generalsx-zh-icon.png`;

interface DashboardState {
  overview: Overview;
  sessions: Session[];
  games: Game[];
}

type LiveStatus = "connecting" | "live" | "reconnecting";

function isSnapshotEvent(value: unknown): value is SnapshotEvent {
  if (!value || typeof value !== "object") {
    return false;
  }
  const candidate = value as Partial<SnapshotEvent>;
  return candidate.type === "snapshot"
    && typeof candidate.sequence === "string"
    && typeof candidate.profile_revision === "string"
    && Boolean(candidate.overview && typeof candidate.overview === "object")
    && Array.isArray(candidate.sessions)
    && Array.isArray(candidate.games);
}

function formatInteger(value: string | number): string {
  try {
    return BigInt(value).toLocaleString();
  } catch {
    return String(value);
  }
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) {
    return "Unknown";
  }
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function formatDuration(value: string): string {
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds < 0) {
    return "Unknown";
  }
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) {
    return `${days}d ${hours}h`;
  }
  if (hours > 0) {
    return `${hours}h ${minutes}m`;
  }
  return `${minutes}m`;
}

function formatBytes(value: string): string {
  const bytes = Number(value);
  if (!Number.isFinite(bytes) || bytes < 0) {
    return value;
  }
  const units = ["B", "KB", "MB", "GB", "TB"];
  let amount = bytes;
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit++;
  }
  return `${amount.toLocaleString(undefined, {maximumFractionDigits: unit === 0 ? 0 : 1})} ${units[unit]}`;
}

function statusColor(status: string): "success" | "warning" | "danger" | "default" {
  switch (status) {
    case "online":
    case "open":
    case "ok":
      return "success";
    case "starting":
    case "in_game":
      return "warning";
    case "closed":
      return "danger";
    default:
      return "default";
  }
}

function StatusChip({status}: {status: string}) {
  return (
    <Chip color={statusColor(status)} size="sm" variant="soft">
      <Chip.Label className="flex items-center gap-1.5">
        {status === "ok" || status === "online" ? <FontAwesomeIcon aria-hidden="true" icon={faCircleCheck} /> : null}
        {status.replaceAll("_", " ")}
      </Chip.Label>
    </Chip>
  );
}

function IconTitle({children, icon}: {children: string; icon: IconDefinition}) {
  return (
    <span className="flex items-center gap-2.5">
      <FontAwesomeIcon aria-hidden="true" className="text-muted" icon={icon} />
      {children}
    </span>
  );
}

function EmptyTableState({children, icon}: {children: string; icon: IconDefinition}) {
  return (
    <div className="flex flex-col items-center gap-3 px-6 py-12 text-center text-sm text-muted">
      <FontAwesomeIcon aria-hidden="true" className="text-xl" icon={icon} />
      <p>{children}</p>
    </div>
  );
}

function ConfirmAction({
  actionLabel,
  actionIcon,
  body,
  heading,
  onConfirm,
}: {
  actionLabel: string;
  actionIcon: IconDefinition;
  body: string;
  heading: string;
  onConfirm: () => void;
}) {
  return (
    <AlertDialog>
      <Button size="sm" variant="danger-soft">
        <FontAwesomeIcon aria-hidden="true" icon={actionIcon} />
        {actionLabel}
      </Button>
      <AlertDialog.Backdrop>
        <AlertDialog.Container>
          <AlertDialog.Dialog>
            <AlertDialog.Header>
              <AlertDialog.Icon status="danger" />
              <AlertDialog.Heading>{heading}</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>{body}</AlertDialog.Body>
            <AlertDialog.Footer>
              <Button slot="close" variant="secondary">Cancel</Button>
              <Button slot="close" variant="danger" onPress={onConfirm}>
                <FontAwesomeIcon aria-hidden="true" icon={actionIcon} />
                {actionLabel}
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </AlertDialog>
  );
}

// GeneralsX @feature OpenAI 02/08/2026 Add accessible profile reset and deletion controls to the admin table.
function ResetPasswordAction({
  onReset,
  profile,
}: {
  onReset: (password: string) => Promise<void>;
  profile: Profile;
}) {
  const [isOpen, setIsOpen] = useState(false);
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [wasSubmitted, setWasSubmitted] = useState(false);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [requestError, setRequestError] = useState("");
  const formID = `reset-password-${profile.user_id}`;
  const passwordBytes = new TextEncoder().encode(password).length;
  const passwordError = wasSubmitted && (passwordBytes < 8 || passwordBytes > 128)
    ? "Password must be 8–128 bytes."
    : "";
  const confirmationError = wasSubmitted && password !== confirmation
    ? "Passwords do not match."
    : "";

  const clearForm = useCallback(() => {
    setPassword("");
    setConfirmation("");
    setWasSubmitted(false);
    setRequestError("");
  }, []);

  const handleOpenChange = useCallback(
    (open: boolean) => {
      setIsOpen(open);
      if (!open) {
        clearForm();
      }
    },
    [clearForm],
  );

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setWasSubmitted(true);
    setRequestError("");
    if (passwordBytes < 8 || passwordBytes > 128 || password !== confirmation) {
      return;
    }
    setIsSubmitting(true);
    try {
      await onReset(password);
      handleOpenChange(false);
    } catch (caught) {
      setRequestError(caught instanceof Error ? caught.message : "The password could not be reset.");
    } finally {
      setIsSubmitting(false);
    }
  }

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setIsOpen(true)}>
        <FontAwesomeIcon aria-hidden="true" icon={faKey} />
        Reset password
      </Button>
      <Modal.Backdrop
        isDismissable={!isSubmitting}
        isKeyboardDismissDisabled={isSubmitting}
        isOpen={isOpen}
        onOpenChange={handleOpenChange}
      >
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-md">
            <Modal.CloseTrigger isDisabled={isSubmitting} />
            <Modal.Header>
              <Modal.Icon className="bg-accent-soft text-accent-soft-foreground">
                <FontAwesomeIcon aria-hidden="true" icon={faKey} />
              </Modal.Icon>
              <Modal.Heading>Reset password</Modal.Heading>
              <p className="mt-1.5 text-sm leading-5 text-muted">
                Set a new password for {profile.display_name}. Their active session and saved login will be revoked.
              </p>
            </Modal.Header>
            <Modal.Body>
              <form className="space-y-4" id={formID} onSubmit={submit}>
                {requestError ? <ErrorAlert message={requestError} /> : null}
                <TextField
                  fullWidth
                  isDisabled={isSubmitting}
                  isInvalid={Boolean(passwordError)}
                  isRequired
                  name="new-password"
                >
                  <Label>New password</Label>
                  <Input
                    autoComplete="new-password"
                    autoFocus
                    placeholder="8–128 bytes"
                    type="password"
                    value={password}
                    variant="secondary"
                    onChange={(event) => setPassword(event.currentTarget.value)}
                  />
                  {passwordError ? <FieldError>{passwordError}</FieldError> : null}
                </TextField>
                <TextField
                  fullWidth
                  isDisabled={isSubmitting}
                  isInvalid={Boolean(confirmationError)}
                  isRequired
                  name="confirm-password"
                >
                  <Label>Confirm new password</Label>
                  <Input
                    autoComplete="new-password"
                    placeholder="Enter it again"
                    type="password"
                    value={confirmation}
                    variant="secondary"
                    onChange={(event) => setConfirmation(event.currentTarget.value)}
                  />
                  {confirmationError ? <FieldError>{confirmationError}</FieldError> : null}
                </TextField>
              </form>
            </Modal.Body>
            <Modal.Footer>
              <Button isDisabled={isSubmitting} variant="secondary" onPress={() => handleOpenChange(false)}>
                Cancel
              </Button>
              <Button form={formID} isPending={isSubmitting} type="submit">
                Reset password
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function ErrorAlert({message}: {message: string}) {
  return (
    <Alert status="danger">
      <Alert.Indicator />
      <Alert.Content>
        <Alert.Title>Request failed</Alert.Title>
        <Alert.Description>{message}</Alert.Description>
      </Alert.Content>
    </Alert>
  );
}

function Login({onAuthenticated}: {onAuthenticated: (token: string) => void}) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [isChecking, setIsChecking] = useState(false);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const candidate = token.trim();
    if (!candidate) {
      setError("Enter the token stored on the server host.");
      return;
    }
    setIsChecking(true);
    setError("");
    try {
      await new AdminApi(candidate).overview();
      sessionStorage.setItem(tokenStorageKey, candidate);
      onAuthenticated(candidate);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "The token could not be verified.");
    } finally {
      setIsChecking(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center px-6 py-12">
      <Card className="w-full max-w-md" variant="secondary">
        <Card.Header className="flex-col items-start gap-4 px-6 pt-6">
          <Chip color="success" size="sm" variant="soft">
            <Chip.Label className="flex items-center gap-1.5">
              <FontAwesomeIcon aria-hidden="true" icon={faShieldHalved} />
              Tailnet only
            </Chip.Label>
          </Chip>
          <div className="flex items-center gap-4">
            <img alt="" aria-hidden="true" className="size-16 shrink-0 rounded-2xl" src={appIconURL} />
            <div className="space-y-2">
              <Card.Title className="text-2xl">GeneralsX Server Admin</Card.Title>
              <Card.Description>
                Authenticate with the bearer token stored on the server. It remains only in this browser tab.
              </Card.Description>
            </div>
          </div>
        </Card.Header>
        <Card.Content className="px-6 py-5">
          <form className="space-y-5" onSubmit={submit}>
            {error ? <ErrorAlert message={error} /> : null}
            <TextField fullWidth isRequired>
              <Label className="flex items-center gap-2">
                <FontAwesomeIcon aria-hidden="true" icon={faKey} />
                Admin token
              </Label>
              <Input
                autoComplete="off"
                autoFocus
                name="admin-token"
                placeholder="Paste the host token"
                type="password"
                value={token}
                variant="secondary"
                onChange={(event) => setToken(event.currentTarget.value)}
              />
            </TextField>
            <Button fullWidth isDisabled={isChecking} type="submit" variant="primary">
              <FontAwesomeIcon aria-hidden="true" icon={faArrowRightToBracket} />
              {isChecking ? "Verifying…" : "Open dashboard"}
            </Button>
          </form>
        </Card.Content>
        <Card.Footer className="border-t border-divider px-6 py-4 text-sm text-muted">
          Access is restricted to the host’s Tailscale address.
        </Card.Footer>
      </Card>
    </main>
  );
}

function MetricCard({label, value, detail, icon}: {label: string; value: number; detail: string; icon: IconDefinition}) {
  return (
    <KPI>
      <KPI.Header>
        <KPI.Title>{label}</KPI.Title>
        <KPI.Icon>
          <FontAwesomeIcon aria-hidden="true" icon={icon} />
        </KPI.Icon>
      </KPI.Header>
      <KPI.Content>
        <KPI.Value value={value} />
      </KPI.Content>
      <KPI.Footer>{detail}</KPI.Footer>
    </KPI>
  );
}

function Dashboard({token, onUnauthorized}: {token: string; onUnauthorized: () => void}) {
  const api = useMemo(() => new AdminApi(token), [token]);
  const [dashboard, setDashboard] = useState<DashboardState | null>(null);
  const [profilePage, setProfilePage] = useState<ProfilePage | null>(null);
  const [profileRevision, setProfileRevision] = useState<string | null>(null);
  const [profileRefreshKey, setProfileRefreshKey] = useState(0);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [offset, setOffset] = useState(0);
  const [error, setError] = useState("");
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [liveStatus, setLiveStatus] = useState<LiveStatus>("connecting");

  const handleError = useCallback(
    (caught: unknown) => {
      if (caught instanceof DOMException && caught.name === "AbortError") {
        return;
      }
      if (caught instanceof ApiError && caught.status === 401) {
        onUnauthorized();
        return;
      }
      setError(caught instanceof Error ? caught.message : "The request could not be completed.");
    },
    [onUnauthorized],
  );

  const loadDashboard = useCallback(
    async (signal?: AbortSignal) => {
      setIsRefreshing(true);
      try {
        const [overview, sessions, games] = await Promise.all([
          api.overview(signal),
          api.sessions(signal),
          api.games(signal),
        ]);
        setDashboard({overview, sessions, games});
        setError("");
      } catch (caught) {
        handleError(caught);
      } finally {
        setIsRefreshing(false);
      }
    },
    [api, handleError],
  );

  const refreshAll = useCallback(async () => {
    setProfileRefreshKey((value) => value + 1);
    await loadDashboard();
  }, [loadDashboard]);

  // GeneralsX @feature OpenAI 02/08/2026 Prefer pushed server snapshots with bounded reconnect and REST fallback.
  useEffect(() => {
    const controller = new AbortController();
    void loadDashboard(controller.signal);
    return () => controller.abort();
  }, [loadDashboard]);

  useEffect(() => {
    if (liveStatus === "live") {
      return;
    }
    const timer = window.setInterval(() => void refreshAll(), 15_000);
    return () => window.clearInterval(timer);
  }, [liveStatus, refreshAll]);

  useEffect(() => {
    let disposed = false;
    let socket: WebSocket | null = null;
    let retryTimer: number | undefined;
    let ticketController: AbortController | null = null;
    let retryAttempt = 0;
    let hasConnected = false;

    function scheduleReconnect() {
      if (disposed) {
        return;
      }
      setLiveStatus(hasConnected ? "reconnecting" : "connecting");
      const delay = Math.min(1_000 * (2 ** retryAttempt), 30_000);
      retryAttempt = Math.min(retryAttempt + 1, 5);
      retryTimer = window.setTimeout(() => void connect(), delay);
    }

    async function connect() {
      if (disposed) {
        return;
      }
      setLiveStatus(hasConnected || retryAttempt > 0 ? "reconnecting" : "connecting");
      ticketController = new AbortController();
      try {
        const ticket = await api.eventTicket(ticketController.signal);
        if (disposed) {
          return;
        }
        const url = new URL("/api/admin/v1/events", window.location.href);
        url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
        url.searchParams.set("ticket", ticket.ticket);

        const nextSocket = new WebSocket(url);
        socket = nextSocket;
        nextSocket.addEventListener("open", () => {
          hasConnected = true;
          retryAttempt = 0;
          setLiveStatus("live");
        });
        nextSocket.addEventListener("message", (event) => {
          if (typeof event.data !== "string") {
            nextSocket.close(1002, "snapshot must be text");
            return;
          }
          try {
            const message: unknown = JSON.parse(event.data);
            if (!isSnapshotEvent(message)) {
              nextSocket.close(1002, "invalid snapshot");
              return;
            }
            setDashboard({overview: message.overview, sessions: message.sessions, games: message.games});
            setProfileRevision(message.profile_revision);
            setError("");
          } catch {
            nextSocket.close(1002, "invalid snapshot");
          }
        });
        nextSocket.addEventListener("error", () => {
          if (nextSocket.readyState < WebSocket.CLOSING) {
            nextSocket.close();
          }
        });
        nextSocket.addEventListener("close", () => {
          if (socket === nextSocket) {
            socket = null;
          }
          scheduleReconnect();
        });
      } catch (caught) {
        if (disposed || (caught instanceof DOMException && caught.name === "AbortError")) {
          return;
        }
        if (caught instanceof ApiError && caught.status === 401) {
          onUnauthorized();
          return;
        }
        scheduleReconnect();
      }
    }

    void connect();
    return () => {
      disposed = true;
      ticketController?.abort();
      if (retryTimer !== undefined) {
        window.clearTimeout(retryTimer);
      }
      socket?.close(1000, "dashboard closed");
    };
  }, [api, onUnauthorized]);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setOffset(0);
      setQuery(queryInput);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [queryInput]);

  useEffect(() => {
    const controller = new AbortController();
    void api
      .profiles(query, profilePageSize, offset, controller.signal)
      .then((page) => {
        setProfilePage(page);
        setError("");
      })
      .catch(handleError);
    return () => controller.abort();
  }, [api, handleError, offset, profileRefreshKey, profileRevision, query]);

  const runAction = useCallback(
    async (action: () => Promise<void>) => {
      try {
        await action();
        await refreshAll();
      } catch (caught) {
        handleError(caught);
      }
    },
    [handleError, refreshAll],
  );

  const resetProfilePassword = useCallback(
    async (profile: Profile, password: string) => {
      try {
        await api.resetPassword(profile.user_id, password);
        await refreshAll();
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 401) {
          handleError(caught);
        }
        throw caught;
      }
    },
    [api, handleError, refreshAll],
  );

  const deleteProfile = useCallback(
    async (profile: Profile) => {
      try {
        await api.deleteProfile(profile.user_id);
        if (offset > 0 && profilePage?.profiles.length === 1) {
          setOffset(Math.max(0, offset - profilePageSize));
        } else {
          setProfileRefreshKey((value) => value + 1);
        }
        await loadDashboard();
      } catch (caught) {
        handleError(caught);
      }
    },
    [api, handleError, loadDashboard, offset, profilePage?.profiles.length],
  );

  const sessionColumns = useMemo<DataGridColumn<Session>[]>(
    () => [
      {
        id: "player",
        header: "Player",
        isRowHeader: true,
        cell: (session) => (
          <div className="min-w-40">
            <p className="font-medium text-foreground">{session.display_name}</p>
            <p className="text-xs text-muted">{session.guest ? "Guest" : session.username || `ID ${session.user_id}`}</p>
          </div>
        ),
      },
      {
        id: "status",
        header: "Status",
        cell: (session) => <StatusChip status={session.status} />,
      },
      {
        id: "location",
        header: "Location",
        cell: (session) => session.game_id ? `Game ${session.game_id.slice(0, 8)}` : session.room_id || "—",
      },
      {
        id: "connected",
        header: "Profile created",
        cell: (session) => <span className="whitespace-nowrap text-muted">{formatDate(session.created_at)}</span>,
      },
      {
        id: "action",
        header: "",
        align: "end",
        cell: (session) => (
          <ConfirmAction
            actionLabel="Disconnect"
            actionIcon={faUserSlash}
            body={`Disconnect ${session.display_name} from the Online server? Their current connection will close immediately.`}
            heading="Disconnect player?"
            onConfirm={() => void runAction(() => api.disconnect(session.user_id))}
          />
        ),
      },
    ],
    [api, runAction],
  );

  const gameColumns = useMemo<DataGridColumn<Game>[]>(
    () => [
      {
        id: "name",
        header: "Lobby",
        isRowHeader: true,
        cell: (game) => (
          <div className="min-w-44">
            <p className="font-medium text-foreground">{game.name}</p>
            <p className="font-mono text-xs text-muted">{game.game_id}</p>
          </div>
        ),
      },
      {id: "state", header: "State", cell: (game) => <StatusChip status={game.state} />},
      {id: "host", header: "Host", accessorKey: "host_name"},
      {
        id: "players",
        header: "Players",
        align: "end",
        cell: (game) => <span className="tabular-nums">{game.players} / {game.max_players}</span>,
      },
      {id: "map", header: "Map", cell: (game) => game.map || "Not selected"},
      {
        id: "action",
        header: "",
        align: "end",
        cell: (game) => (
          <ConfirmAction
            actionLabel="Close lobby"
            actionIcon={faCircleXmark}
            body={`Close ${game.name} and remove every player from this lobby?`}
            heading="Close game lobby?"
            onConfirm={() => void runAction(() => api.closeGame(game.game_id))}
          />
        ),
      },
    ],
    [api, runAction],
  );

  const profileColumns = useMemo<DataGridColumn<Profile>[]>(
    () => [
      {
        id: "display_name",
        header: "Profile",
        isRowHeader: true,
        accessorKey: "display_name",
        cell: (profile) => (
          <div className="min-w-44">
            <p className="font-medium text-foreground">{profile.display_name}</p>
            <p className="text-xs text-muted">{profile.username}</p>
          </div>
        ),
      },
      {
        id: "rating",
        header: "Rating",
        align: "end",
        accessorKey: "rating",
        cell: (profile) => <span className="tabular-nums">{formatInteger(profile.rating)}</span>,
      },
      {
        id: "record",
        header: "W / L / DC",
        align: "end",
        cell: (profile) => (
          <span className="whitespace-nowrap tabular-nums">
            {formatInteger(profile.wins)} / {formatInteger(profile.losses)} / {formatInteger(profile.disconnects)}
          </span>
        ),
      },
      {
        id: "games",
        header: "Games",
        align: "end",
        accessorKey: "games",
        cell: (profile) => <span className="tabular-nums">{formatInteger(profile.games)}</span>,
      },
      {
        id: "created_at",
        header: "Joined",
        accessorKey: "created_at",
        cell: (profile) => <span className="whitespace-nowrap text-muted">{formatDate(profile.created_at)}</span>,
      },
      {
        id: "actions",
        header: "Actions",
        align: "end",
        cell: (profile) => (
          <div className="flex items-center justify-end gap-2 whitespace-nowrap">
            <ResetPasswordAction
              profile={profile}
              onReset={(password) => resetProfilePassword(profile, password)}
            />
            <ConfirmAction
              actionLabel="Delete"
              actionIcon={faTrashCan}
              body={`Permanently delete ${profile.display_name}? Their active session and saved login will be revoked. This cannot be undone.`}
              heading="Delete profile?"
              onConfirm={() => void deleteProfile(profile)}
            />
          </div>
        ),
      },
    ],
    [deleteProfile, resetProfilePassword],
  );

  const overview = dashboard?.overview;
  const profileTotal = profilePage ? Number(profilePage.total) : 0;
  const hasPreviousProfiles = offset > 0;
  const hasNextProfiles = profilePage ? offset + profilePage.profiles.length < profileTotal : false;
  const liveStatusLabel = liveStatus === "live"
    ? "Live updates"
    : liveStatus === "reconnecting"
      ? "Reconnecting"
      : "Connecting";

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="border-b border-divider bg-background/95 backdrop-blur">
        <div className="mx-auto flex max-w-7xl flex-col gap-4 px-6 py-5 sm:flex-row sm:items-center sm:justify-between lg:px-8">
          <div className="flex items-center gap-3.5">
            <img alt="" aria-hidden="true" className="size-11 shrink-0 rounded-xl" src={appIconURL} />
            <div>
              <div className="flex items-center gap-3">
                <h1 className="text-xl font-semibold tracking-tight">GeneralsX Server Admin</h1>
                <span aria-live="polite">
                  <Chip color={liveStatus === "live" ? "success" : "warning"} size="sm" variant="soft">
                    <Chip.Label className="flex items-center gap-1.5">
                      <FontAwesomeIcon
                        aria-hidden="true"
                        className={liveStatus === "reconnecting" ? "animate-spin motion-reduce:animate-none" : undefined}
                        icon={liveStatus === "live" ? faCircleCheck : liveStatus === "reconnecting" ? faArrowsRotate : faClock}
                      />
                      {liveStatusLabel}
                    </Chip.Label>
                  </Chip>
                </span>
              </div>
              <p className="mt-1 text-sm text-muted">
                {overview ? `Protocol ${overview.protocol} · Up ${formatDuration(overview.uptime_seconds)}` : "Loading server state…"}
              </p>
            </div>
          </div>
          <div className="flex gap-2">
            <Button isDisabled={isRefreshing} variant="secondary" onPress={() => void refreshAll()}>
              <FontAwesomeIcon
                aria-hidden="true"
                className={isRefreshing ? "animate-spin motion-reduce:animate-none" : undefined}
                icon={faArrowsRotate}
              />
              {isRefreshing ? "Refreshing…" : "Refresh"}
            </Button>
            <Button variant="tertiary" onPress={onUnauthorized}>
              <FontAwesomeIcon aria-hidden="true" icon={faRightFromBracket} />
              End session
            </Button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-7xl space-y-8 px-6 py-8 lg:px-8">
        {error ? <ErrorAlert message={error} /> : null}

        <section aria-labelledby="overview-heading" className="space-y-4">
          <div>
            <h2 id="overview-heading" className="flex items-center gap-2.5 text-lg font-semibold">
              <FontAwesomeIcon aria-hidden="true" className="text-muted" icon={faGaugeHigh} />
              Overview
            </h2>
            <p className="mt-1 text-sm text-muted">
              {liveStatus === "live"
                ? "Live Online service health and capacity. Updates stream from the server."
                : liveStatus === "reconnecting"
                  ? "Live connection interrupted. REST fallback refreshes every 15 seconds while it reconnects."
                  : "Connecting to live updates. REST fallback refreshes every 15 seconds."}
            </p>
          </div>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <MetricCard
              detail="Persistent SQLite profiles"
              icon={faDatabase}
              label="Profiles"
              value={Number(overview?.profile_count || 0)}
            />
            <MetricCard
              detail={`${overview?.hub.queued_players || 0} waiting in Quick Match`}
              icon={faUsers}
              label="Online players"
              value={overview?.hub.online_players || 0}
            />
            <MetricCard
              detail="Joinable staged games"
              icon={faDoorOpen}
              label="Open lobbies"
              value={overview?.hub.open_games || 0}
            />
            <MetricCard
              detail="Starting or launched matches"
              icon={faGamepad}
              label="Active games"
              value={overview?.hub.active_games || 0}
            />
          </div>
        </section>

        <section aria-labelledby="sessions-heading">
          <Card variant="secondary">
            <Card.Header className="flex-col items-start gap-1 px-6 pt-6">
              <Card.Title id="sessions-heading"><IconTitle icon={faTowerBroadcast}>Live sessions</IconTitle></Card.Title>
              <Card.Description>Authenticated and guest players currently connected to the control service.</Card.Description>
            </Card.Header>
            <Card.Content className="px-0 pb-0 pt-4">
              <DataGrid
                aria-label="Live sessions"
                columns={sessionColumns}
                contentClassName="min-w-[820px]"
                data={dashboard?.sessions || []}
                getRowId={(session) => session.user_id}
                renderEmptyState={() => <EmptyTableState icon={faUsersSlash}>No players are online.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </Card.Content>
          </Card>
        </section>

        <section aria-labelledby="relay-heading">
          <Card variant="secondary">
            <Card.Header className="flex-col items-start gap-1 px-6 pt-6">
              <Card.Title id="relay-heading"><IconTitle icon={faSatelliteDish}>Relay activity</IconTitle></Card.Title>
              <Card.Description>Opaque UDP traffic handled by the authenticated game relay.</Card.Description>
            </Card.Header>
            <Card.Content className="grid gap-x-8 gap-y-5 px-6 py-6 sm:grid-cols-2 lg:grid-cols-4">
              {[
                {icon: faArrowDown, label: "Inbound", value: `${formatInteger(overview?.relay.datagrams_in || "0")} packets · ${formatBytes(overview?.relay.bytes_in || "0")}`},
                {icon: faArrowUp, label: "Outbound", value: `${formatInteger(overview?.relay.datagrams_out || "0")} packets · ${formatBytes(overview?.relay.bytes_out || "0")}`},
                {icon: faShieldHalved, label: "Auth drops", value: formatInteger(overview?.relay.dropped_auth || "0")},
                {icon: faGaugeHigh, label: "Rate-limit drops", value: formatInteger(overview?.relay.dropped_rate_limit || "0")},
                {icon: faTriangleExclamation, label: "Malformed", value: formatInteger(overview?.relay.dropped_malformed || "0")},
                {icon: faPlugCircleXmark, label: "No endpoint", value: formatInteger(overview?.relay.dropped_no_endpoint || "0")},
                {icon: faClock, label: "Buffered before bind", value: formatInteger(overview?.relay.buffered_until_bind || "0")},
                {icon: faLayerGroup, label: "Allocated games", value: formatInteger(overview?.relay.active_games || 0)},
              ].map(({icon, label, value}) => (
                <div className="flex gap-3" key={label}>
                  <FontAwesomeIcon aria-hidden="true" className="mt-0.5 text-muted" fixedWidth icon={icon} />
                  <div>
                    <p className="text-sm text-muted">{label}</p>
                    <p className="mt-1 font-medium tabular-nums text-foreground">{value}</p>
                  </div>
                </div>
              ))}
            </Card.Content>
          </Card>
        </section>

        <section aria-labelledby="games-heading">
          <Card variant="secondary">
            <Card.Header className="flex-col items-start gap-1 px-6 pt-6">
              <Card.Title id="games-heading"><IconTitle icon={faGamepad}>Game lobbies</IconTitle></Card.Title>
              <Card.Description>Open, starting, and active games held by the matchmaking hub.</Card.Description>
            </Card.Header>
            <Card.Content className="px-0 pb-0 pt-4">
              <DataGrid
                aria-label="Game lobbies"
                columns={gameColumns}
                contentClassName="min-w-[900px]"
                data={dashboard?.games || []}
                getRowId={(game) => game.game_id}
                renderEmptyState={() => <EmptyTableState icon={faGamepad}>No game lobbies are open.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </Card.Content>
          </Card>
        </section>

        <section aria-labelledby="profiles-heading">
          <Card variant="secondary">
            <Card.Header className="flex-col gap-4 px-6 pt-6 sm:flex-row sm:items-end sm:justify-between">
              <div className="space-y-1">
                <Card.Title id="profiles-heading"><IconTitle icon={faAddressCard}>Profiles</IconTitle></Card.Title>
                <Card.Description>{formatInteger(profilePage?.total || "0")} stored accounts match this view.</Card.Description>
              </div>
              <SearchField
                aria-label="Search profiles"
                className="w-full sm:max-w-xs"
                variant="secondary"
                value={queryInput}
                onChange={setQueryInput}
              >
                <SearchField.Group>
                  <SearchField.SearchIcon />
                  <SearchField.Input placeholder="Search profiles" />
                  <SearchField.ClearButton />
                </SearchField.Group>
              </SearchField>
            </Card.Header>
            <Card.Content className="px-0 pb-0 pt-4">
              <DataGrid
                aria-label="Stored profiles"
                columns={profileColumns}
                contentClassName="min-w-[1080px]"
                data={profilePage?.profiles || []}
                getRowId={(profile) => profile.user_id}
                renderEmptyState={() => <EmptyTableState icon={faMagnifyingGlass}>No profiles match this search.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </Card.Content>
            <Card.Footer className="flex items-center justify-between border-t border-divider px-6 py-4">
              <p className="text-sm text-muted">
                {profilePage && profilePage.profiles.length > 0
                  ? `${offset + 1}–${offset + profilePage.profiles.length} of ${formatInteger(profilePage.total)}`
                  : "No results"}
              </p>
              <div className="flex gap-2">
                <Button
                  isDisabled={!hasPreviousProfiles}
                  size="sm"
                  variant="secondary"
                  onPress={() => setOffset(Math.max(0, offset - profilePageSize))}
                >
                  <FontAwesomeIcon aria-hidden="true" icon={faChevronLeft} />
                  Previous
                </Button>
                <Button
                  isDisabled={!hasNextProfiles}
                  size="sm"
                  variant="secondary"
                  onPress={() => setOffset(offset + profilePageSize)}
                >
                  Next
                  <FontAwesomeIcon aria-hidden="true" icon={faChevronRight} />
                </Button>
              </div>
            </Card.Footer>
          </Card>
        </section>

      </main>
    </div>
  );
}

export function App() {
  const [token, setToken] = useState(() => sessionStorage.getItem(tokenStorageKey) || "");

  const clearToken = useCallback(() => {
    sessionStorage.removeItem(tokenStorageKey);
    setToken("");
  }, []);

  if (!token) {
    return <Login onAuthenticated={setToken} />;
  }
  return <Dashboard token={token} onUnauthorized={clearToken} />;
}
