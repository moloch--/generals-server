import type {IconDefinition} from "@fortawesome/fontawesome-svg-core";
import {
  faArrowUpRightFromSquare,
  faArrowsRotate,
  faBars,
  faCircleCheck,
  faClock,
  faDownload,
  faDoorOpen,
  faGamepad,
  faGaugeHigh,
  faLaptopCode,
  faListCheck,
  faPlay,
  faShieldHalved,
  faTriangleExclamation,
  faUsers,
  faUsersSlash,
  faXmark,
} from "@fortawesome/free-solid-svg-icons";
import {FontAwesomeIcon} from "@fortawesome/react-fontawesome";
import {Alert} from "@heroui/react/alert";
import {Button} from "@heroui/react/button";
import {Card} from "@heroui/react/card";
import {Chip} from "@heroui/react/chip";
import {Link} from "@heroui/react/link";
import {Skeleton} from "@heroui/react/skeleton";
import {buttonVariants} from "@heroui/styles/components/button";
import {memo, useEffect, useMemo, useState} from "react";

import {DataGrid, type DataGridColumn} from "../components/DataGrid";
import {
  type ActiveGame,
  fetchPublicSnapshot,
  type LeaderboardEntry,
  type OnlinePlayer,
  type PublicLobby,
  type PublicSnapshot,
} from "./api";
import {type PublicRoute, PublicRouteLink, usePublicRouter} from "./router";

const pollIntervalMilliseconds = 10_000;
const staleAfterMilliseconds = 30_000;
const appIconURL = `${import.meta.env.BASE_URL}generalsx-zh-icon.png`;

const navigationItems = [
  {label: "Overview", path: "/"},
  {label: "Leaderboard", path: "/leaderboard"},
  {label: "Game Lobbies", path: "/game-lobbies"},
  {label: "Online Players", path: "/online-players"},
  {label: "Active Games", path: "/active-games"},
  {label: "How to play", path: "/how-to-play"},
] as const satisfies ReadonlyArray<{label: string; path: PublicRoute}>;

type ActivityStatus = "connecting" | "delayed" | "live";

interface RankedPlayer extends LeaderboardEntry {
  rank: number;
}

interface LobbyRow extends PublicLobby {
  rowKey: string;
}

interface ActiveGameRow extends ActiveGame {
  rowKey: string;
}

function formatInteger(value: string | number): string {
  try {
    return BigInt(value).toLocaleString();
  } catch {
    return String(value);
  }
}

function formatStatus(value: string): string {
  return value
    .replaceAll("_", " ")
    .replace(/\b\w/g, (character) => character.toUpperCase());
}

function formatUpdatedAt(value: string): string {
  const generatedAt = new Date(value);
  if (Number.isNaN(generatedAt.valueOf())) {
    return "Update time unavailable";
  }
  return `Updated ${new Intl.DateTimeFormat(undefined, {
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
  }).format(generatedAt)}`;
}

function statusColor(status: string): "success" | "accent" | "default" {
  switch (status.toLowerCase()) {
    case "online":
    case "open":
    case "started":
      return "success";
    case "in_lobby":
    case "in_game":
    case "starting":
    case "queued":
    case "quick_match":
      return "accent";
    default:
      return "default";
  }
}

function StatusChip({status}: {status: string}) {
  return (
    <Chip color={statusColor(status)} size="sm" variant="soft">
      <Chip.Label>{formatStatus(status)}</Chip.Label>
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

function MetricCard({detail, icon, label, value}: {
  detail: string;
  icon: IconDefinition;
  label: string;
  value: number;
}) {
  return (
    <Card className="h-full" variant="secondary">
      <Card.Content className="flex h-full flex-col gap-4 p-6">
        <div className="flex items-center justify-between gap-3">
          <p className="text-sm text-muted">{label}</p>
          <FontAwesomeIcon aria-hidden="true" className="text-muted" icon={icon} />
        </div>
        <p className="text-3xl font-semibold tabular-nums tracking-tight">{value.toLocaleString()}</p>
        <p className="mt-auto text-sm text-muted">{detail}</p>
      </Card.Content>
    </Card>
  );
}

function DashboardSkeleton() {
  return (
    <div aria-label="Loading server activity" className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4" role="status">
      {Array.from({length: 4}, (_, index) => (
        <Card key={index} variant="secondary">
          <Card.Content className="space-y-5 p-6">
            <Skeleton className="h-4 w-28 rounded-lg" />
            <Skeleton className="h-9 w-16 rounded-lg" />
            <Skeleton className="h-4 w-36 rounded-lg" />
          </Card.Content>
        </Card>
      ))}
    </div>
  );
}

function TableLoadingState() {
  return (
    <div aria-label="Loading table data" className="space-y-3 px-6 py-8" role="status">
      {Array.from({length: 3}, (_, index) => (
        <Skeleton className="h-10 w-full rounded-xl" key={index} />
      ))}
    </div>
  );
}

function SectionCard({children, description, heading, headingId, icon}: {
  children: React.ReactNode;
  description: string;
  heading: string;
  headingId: string;
  icon: IconDefinition;
}) {
  return (
    <Card variant="secondary">
      <Card.Header className="flex-col items-start gap-1 px-6 pt-6">
        <Card.Title
          className="focus:outline-none"
          id={headingId}
          render={(props) => <h1 {...props} tabIndex={-1} />}
        >
          <IconTitle icon={icon}>{heading}</IconTitle>
        </Card.Title>
        <Card.Description>{description}</Card.Description>
      </Card.Header>
      <Card.Content className="px-0 pb-0 pt-4">{children}</Card.Content>
    </Card>
  );
}

function PageHeading({description, heading, headingId, icon}: {
  description: string;
  heading: string;
  headingId: string;
  icon: IconDefinition;
}) {
  return (
    <div>
      <h1
        className="flex items-center gap-2.5 text-2xl font-semibold tracking-tight focus:outline-none"
        id={headingId}
        tabIndex={-1}
      >
        <FontAwesomeIcon aria-hidden="true" className="text-muted" icon={icon} />
        {heading}
      </h1>
      <p className="mt-2 max-w-2xl text-sm text-muted">{description}</p>
    </div>
  );
}

function ScrollToRoute() {
  const {route} = usePublicRouter();

  useEffect(() => {
    const pageLabel = navigationItems.find((item) => item.path === route)?.label;
    document.title = route === "/" || !pageLabel
      ? "GeneralsX Online"
      : `${pageLabel} · GeneralsX Online`;
    window.scrollTo(0, 0);
    const frame = window.requestAnimationFrame(() => {
      document.querySelector<HTMLElement>("main h1")?.focus({preventScroll: true});
    });
    return () => window.cancelAnimationFrame(frame);
  }, [route]);

  return null;
}

const PublicNavigation = memo(function PublicNavigation({status}: {status: ActivityStatus}) {
  const [isMenuOpen, setIsMenuOpen] = useState(false);
  const {route} = usePublicRouter();

  useEffect(() => setIsMenuOpen(false), [route]);

  const statusLabel = status === "connecting" ? "Connecting" : status === "delayed" ? "Update delayed" : "Live";
  const statusIcon = status === "connecting" ? faArrowsRotate : status === "delayed" ? faTriangleExclamation : faCircleCheck;

  return (
    <header className="sticky top-0 z-40 border-b border-divider bg-background/90 backdrop-blur-xl">
      <div className="mx-auto flex h-16 max-w-7xl items-center gap-4 px-6 lg:px-8">
        <button
          aria-controls="public-navigation-menu"
          aria-expanded={isMenuOpen}
          aria-label={isMenuOpen ? "Close navigation" : "Open navigation"}
          className="-ml-2 flex size-10 cursor-[var(--cursor-interactive)] items-center justify-center rounded-xl text-muted hover:bg-default/10 hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus xl:hidden"
          type="button"
          onClick={() => setIsMenuOpen((value) => !value)}
        >
          <FontAwesomeIcon aria-hidden="true" icon={isMenuOpen ? faXmark : faBars} />
        </button>
        <PublicRouteLink
          aria-label="GeneralsX Online home"
          className="flex shrink-0 items-center gap-3 no-underline"
          to="/"
          onClick={() => setIsMenuOpen(false)}
        >
          <img alt="" aria-hidden="true" className="size-9 rounded-xl" src={appIconURL} />
          <span className="hidden font-semibold tracking-tight text-foreground sm:inline">GeneralsX Online</span>
        </PublicRouteLink>
        <nav aria-label="Primary navigation" className="ml-auto hidden items-center gap-1 xl:flex">
          {navigationItems.map((item) => {
            const isActive = route === item.path;
            return (
              <PublicRouteLink
                aria-current={isActive ? "page" : undefined}
                className={`rounded-xl px-3 py-2 text-sm font-medium no-underline ${
                  isActive
                  ? "bg-accent-soft text-accent-soft-foreground shadow-sm"
                  : "text-muted hover:bg-default/10 hover:text-foreground"
                }`}
                key={item.path}
                to={item.path}
              >
                {item.label}
              </PublicRouteLink>
            );
          })}
        </nav>
        <div className="ml-auto w-8 shrink-0 sm:w-28 xl:ml-3">
          <span aria-label={statusLabel} aria-live="polite" className="block" role="status">
            <Chip
              className="w-8 justify-center px-0 sm:w-full sm:px-2"
              color={status === "live" ? "success" : "warning"}
              size="sm"
              variant="soft"
            >
              <Chip.Label className="flex items-center justify-center gap-1.5">
                <FontAwesomeIcon
                  aria-hidden="true"
                  className={`size-3.5 shrink-0 ${status === "connecting" ? "animate-spin motion-reduce:animate-none" : ""}`}
                  icon={statusIcon}
                />
                <span className="hidden sm:inline">{statusLabel}</span>
              </Chip.Label>
            </Chip>
          </span>
        </div>
      </div>
      {isMenuOpen ? (
        <nav aria-label="Mobile navigation" className="border-t border-divider px-6 py-3 xl:hidden" id="public-navigation-menu">
          <div className="mx-auto grid max-w-7xl gap-1">
            {navigationItems.map((item) => {
              const isActive = route === item.path;
              return (
                <PublicRouteLink
                  aria-current={isActive ? "page" : undefined}
                  className={`rounded-xl px-3 py-2.5 text-sm font-medium no-underline ${
                    isActive
                    ? "bg-accent-soft text-accent-soft-foreground shadow-sm"
                    : "text-muted hover:bg-default/10 hover:text-foreground"
                  }`}
                  key={item.path}
                  to={item.path}
                  onClick={() => setIsMenuOpen(false)}
                >
                  {item.label}
                </PublicRouteLink>
              );
            })}
          </div>
        </nav>
      ) : null}
    </header>
  );
});

const latestReleaseURL = "https://github.com/moloch--/Generals/releases/latest";

const platformDownloads = [
  {
    description: "Apple Silicon desktop app. The build tool is ad-hoc signed and not notarized.",
    href: "https://github.com/moloch--/Generals/releases/download/v0.0.5/generalsx-build-desktop-v0.0.5-macos-arm64.dmg",
    label: "Download macOS DMG",
    platform: "macOS Apple Silicon",
  },
  {
    description: "Native x86-64 desktop app. Packaged launch verification is established; rendered gameplay remains exploratory.",
    href: "https://github.com/moloch--/Generals/releases/download/v0.0.5/generalsx-build-desktop-v0.0.5-windows-amd64.exe",
    label: "Download Windows app",
    platform: "Windows x86-64",
  },
  {
    description: "Native x86-64 app requiring WebKitGTK 4.1. Built games need Vulkan and glibc 2.38 or newer.",
    href: "https://github.com/moloch--/Generals/releases/download/v0.0.5/generalsx-build-desktop-v0.0.5-linux-amd64",
    label: "Download Linux app",
    platform: "Linux x86-64",
  },
] as const;

const buildSteps = [
  {
    heading: "Bring your game files",
    text: "You need a legal copy of Command & Conquer: Generals – Zero Hour. The tool contains no retail game data; Steam downloads require an account that owns app 2732960.",
  },
  {
    heading: "Run the guided setup",
    text: "Choose a supported target, use an existing source checkout or automatic clone, select owned game files or SteamCMD, then review the output settings and start the build.",
  },
  {
    heading: "Keep credentials in the terminal",
    text: "When SteamCMD asks for a password or Steam Guard challenge, enter it only in the native terminal it opens. The desktop app does not receive or store those credentials.",
  },
  {
    heading: "Copy the finished game",
    text: "After a successful build, choose Copy to Desktop. macOS receives GeneralsXZH.app; Windows and Linux receive their native self-extracting executable.",
  },
  {
    heading: "Join Online multiplayer",
    text: "Launch the game and choose MULTIPLAYER, then ONLINE — not NETWORK. Create an account or sign in, enter a room, and create or join a game.",
  },
] as const;

function HowToPlayPage() {
  return (
    <section aria-labelledby="how-to-play-heading" className="space-y-6">
      <PageHeading
        description="Build a native GeneralsX client from your legally owned Zero Hour files, then connect through the built-in Online service."
        heading="How to play"
        headingId="how-to-play-heading"
        icon={faLaptopCode}
      />

      <Card variant="tertiary">
        <Card.Content className="flex flex-col items-start gap-5 p-6 sm:p-8">
          <div className="flex items-start gap-3">
            <FontAwesomeIcon aria-hidden="true" className="mt-1 text-accent" icon={faDownload} />
            <div>
              <h2 className="text-lg font-semibold">Get the Automated Build Tool</h2>
              <p className="mt-1 max-w-2xl text-sm text-muted">
                Start with the latest official release. Versioned platform downloads for v0.0.5 are listed below.
              </p>
            </div>
          </div>
          <Link
            className={buttonVariants({variant: "primary"})}
            href={latestReleaseURL}
            rel="noopener noreferrer"
            target="_blank"
          >
            <FontAwesomeIcon aria-hidden="true" icon={faDownload} />
            Open latest release
          </Link>
        </Card.Content>
      </Card>

      <div className="grid gap-4 lg:grid-cols-3">
        {platformDownloads.map((download) => (
          <Card className="h-full" key={download.platform} variant="secondary">
            <Card.Content className="flex h-full flex-col items-start gap-3 p-6">
              <h2 className="font-semibold">{download.platform}</h2>
              <p className="text-sm text-muted">{download.description}</p>
              <Link
                className="mt-auto"
                href={download.href}
                rel="noopener noreferrer"
                target="_blank"
              >
                {download.label}
                <Link.Icon>
                  <FontAwesomeIcon aria-hidden="true" icon={faArrowUpRightFromSquare} />
                </Link.Icon>
              </Link>
            </Card.Content>
          </Card>
        ))}
      </div>

      <Card variant="secondary">
        <Card.Header className="flex-col items-start gap-1 px-6 pt-6">
          <Card.Title className="flex items-center gap-2.5">
            <FontAwesomeIcon aria-hidden="true" className="text-muted" icon={faListCheck} />
            Build and launch
          </Card.Title>
          <Card.Description>The guided tool handles the platform-specific build steps.</Card.Description>
        </Card.Header>
        <Card.Content className="px-6 py-6">
          <ol className="grid gap-5 lg:grid-cols-2">
            {buildSteps.map((step, index) => (
              <li className="flex items-start gap-4" key={step.heading}>
                <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-default-soft text-sm font-semibold tabular-nums text-default-soft-foreground">
                  {index + 1}
                </span>
                <div>
                  <h3 className="font-medium">{step.heading}</h3>
                  <p className="mt-1 text-sm text-muted">{step.text}</p>
                </div>
              </li>
            ))}
          </ol>
        </Card.Content>
      </Card>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card variant="secondary">
          <Card.Content className="flex items-start gap-4 p-6">
            <FontAwesomeIcon aria-hidden="true" className="mt-1 text-success" icon={faPlay} />
            <div>
              <h2 className="font-semibold">Online is preconfigured</h2>
              <p className="mt-1 text-sm text-muted">
                Generated clients default to <code className="font-mono text-foreground">tls://multiplayer.generals.network</code>.
              </p>
            </div>
          </Card.Content>
        </Card>
        <Card variant="secondary">
          <Card.Content className="flex items-start gap-4 p-6">
            <FontAwesomeIcon aria-hidden="true" className="mt-1 text-warning" icon={faShieldHalved} />
            <div>
              <h2 className="font-semibold">Keep retail data private</h2>
              <p className="mt-1 text-sm text-muted">
                Generated game artifacts contain your personal retail files. Do not redistribute them.
              </p>
            </div>
          </Card.Content>
        </Card>
      </div>

      <div className="flex flex-wrap gap-x-5 gap-y-2 text-sm">
        <Link href="https://github.com/moloch--/Generals/blob/main/docs/HOWTO/AUTOMATED_SFX_BUILD.md" rel="noopener noreferrer" target="_blank">
          Automated Build Tool guide
          <Link.Icon />
        </Link>
        <Link href="https://github.com/moloch--/Generals/blob/main/docs/HOWTO/ONLINE_MULTIPLAYER.md#join-or-host-a-match" rel="noopener noreferrer" target="_blank">
          Online multiplayer guide
          <Link.Icon />
        </Link>
        <Link href="https://github.com/moloch--/Generals/releases/download/v0.0.5/SHA256SUMS" rel="noopener noreferrer" target="_blank">
          v0.0.5 checksums
          <Link.Icon />
        </Link>
      </div>
    </section>
  );
}

export function PublicApp() {
  const {route} = usePublicRouter();
  const [snapshot, setSnapshot] = useState<PublicSnapshot | null>(null);
  const [error, setError] = useState("");
  const [lastSuccessAt, setLastSuccessAt] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  const [refreshKey, setRefreshKey] = useState(0);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 5_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    let disposed = false;
    let timer: number | undefined;
    let controller: AbortController | undefined;

    const schedule = () => {
      if (timer !== undefined) {
        window.clearTimeout(timer);
      }
      timer = window.setTimeout(() => void load(), pollIntervalMilliseconds);
    };

    const load = async () => {
      if (disposed || document.visibilityState !== "visible") {
        return;
      }
      controller?.abort();
      controller = new AbortController();
      try {
        const nextSnapshot = await fetchPublicSnapshot(controller.signal);
        if (!disposed) {
          setSnapshot(nextSnapshot);
          setLastSuccessAt(Date.now());
          setError("");
        }
      } catch (caught) {
        if (!disposed && !(caught instanceof DOMException && caught.name === "AbortError")) {
          setError(caught instanceof Error ? caught.message : "Server activity could not be loaded.");
        }
      } finally {
        if (!disposed) {
          schedule();
        }
      }
    };

    const handleVisibilityChange = () => {
      if (timer !== undefined) {
        window.clearTimeout(timer);
        timer = undefined;
      }
      if (document.visibilityState === "visible") {
        void load();
      } else {
        controller?.abort();
      }
    };

    document.addEventListener("visibilitychange", handleVisibilityChange);
    void load();
    return () => {
      disposed = true;
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      if (timer !== undefined) {
        window.clearTimeout(timer);
      }
      controller?.abort();
    };
  }, [refreshKey]);

  const rankedPlayers = useMemo<RankedPlayer[]>(
    () => (snapshot?.leaderboard || []).map((player, index) => ({...player, rank: index + 1})),
    [snapshot?.leaderboard],
  );

  const lobbyRows = useMemo<LobbyRow[]>(
    () => (snapshot?.lobbies || []).map((lobby, index) => ({...lobby, rowKey: `lobby-${index}`})),
    [snapshot?.lobbies],
  );

  const activeGameRows = useMemo<ActiveGameRow[]>(
    () => (snapshot?.active_games || []).map((game, index) => ({...game, rowKey: `active-game-${index}`})),
    [snapshot?.active_games],
  );

  const leaderboardColumns = useMemo<DataGridColumn<RankedPlayer>[]>(
    () => [
      {id: "rank", header: "Rank", align: "end", cell: (player) => <span className="tabular-nums text-muted">{player.rank}</span>},
      {id: "player", header: "Player", isRowHeader: true, accessorKey: "display_name", cell: (player) => <span className="font-medium text-foreground">{player.display_name}</span>},
      {id: "rating", header: "Rating", align: "end", accessorKey: "rating", cell: (player) => <span className="tabular-nums">{formatInteger(player.rating)}</span>},
      {id: "record", header: "W / L", align: "end", cell: (player) => <span className="whitespace-nowrap tabular-nums">{formatInteger(player.wins)} / {formatInteger(player.losses)}</span>},
      {id: "games", header: "Games", align: "end", accessorKey: "games", cell: (player) => <span className="tabular-nums">{formatInteger(player.games)}</span>},
    ],
    [],
  );

  const lobbyColumns = useMemo<DataGridColumn<LobbyRow>[]>(
    () => [
      {id: "lobby", header: "Lobby", isRowHeader: true, cell: (lobby) => <div className="min-w-40"><p className="font-medium text-foreground">{lobby.name}</p><p className="text-xs text-muted">{lobby.map || "Map not selected"}</p></div>},
      {id: "host", header: "Host", accessorKey: "host_name"},
      {id: "players", header: "Players", align: "end", cell: (lobby) => <span className="tabular-nums">{lobby.players} / {lobby.max_players}</span>},
      {id: "access", header: "Access", cell: (lobby) => <Chip color={lobby.has_password ? "warning" : "success"} size="sm" variant="soft"><Chip.Label>{lobby.has_password ? "Password" : "Open"}</Chip.Label></Chip>},
      {id: "product", header: "Game", accessorKey: "product"},
    ],
    [],
  );

  const playerColumns = useMemo<DataGridColumn<OnlinePlayer>[]>(
    () => [
      {id: "player", header: "Player", isRowHeader: true, accessorKey: "display_name", cell: (player) => <span className="font-medium text-foreground">{player.display_name}</span>},
      {id: "status", header: "Status", align: "end", cell: (player) => <StatusChip status={player.status} />},
    ],
    [],
  );

  const activeGameColumns = useMemo<DataGridColumn<ActiveGameRow>[]>(
    () => [
      {id: "game", header: "Game", isRowHeader: true, cell: (game) => <div className="min-w-40"><p className="font-medium text-foreground">{game.name}</p><p className="text-xs text-muted">{game.map || "Map not selected"}</p></div>},
      {id: "state", header: "State", cell: (game) => <StatusChip status={game.state} />},
      {id: "players", header: "Players", align: "end", cell: (game) => <span className="tabular-nums">{game.players} / {game.max_players}</span>},
      {id: "product", header: "Game", accessorKey: "product"},
    ],
    [],
  );

  const isStale = Boolean(snapshot && (error || (lastSuccessAt > 0 && now - lastSuccessAt >= staleAfterMilliseconds)));
  const activityStatus: ActivityStatus = !snapshot ? "connecting" : isStale ? "delayed" : "live";

  return (
    <div className="min-h-screen bg-background text-foreground">
      <PublicNavigation status={activityStatus} />
      <ScrollToRoute />

      <main className="mx-auto max-w-7xl px-6 py-8 lg:px-8">
        {route === "/" ? (
          <section aria-labelledby="overview-heading" className="space-y-5">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
              <div>
                <h1
                  className="flex items-center gap-2.5 text-2xl font-semibold tracking-tight focus:outline-none"
                  id="overview-heading"
                  tabIndex={-1}
                >
                  <FontAwesomeIcon aria-hidden="true" className="text-muted" icon={faGaugeHigh} />
                  Server Overview
                </h1>
                <p className="mt-2 max-w-2xl text-sm text-muted">
                  Live community activity across matchmaking, lobbies, and games.
                </p>
              </div>
              <p className="text-sm tabular-nums text-muted">
                {snapshot ? formatUpdatedAt(snapshot.generated_at) : "Waiting for the first update"}
              </p>
            </div>

            {error ? (
              <Alert status={snapshot ? "warning" : "danger"}>
                <Alert.Indicator />
                <Alert.Content>
                  <Alert.Title>{snapshot ? "Live updates are delayed" : "Server activity is unavailable"}</Alert.Title>
                  <Alert.Description>{error}</Alert.Description>
                </Alert.Content>
                <Button variant="secondary" onPress={() => setRefreshKey((value) => value + 1)}>
                  <FontAwesomeIcon aria-hidden="true" icon={faArrowsRotate} />
                  Retry
                </Button>
              </Alert>
            ) : null}

            {!snapshot ? <DashboardSkeleton /> : (
              <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
                <MetricCard detail="Connected right now" icon={faUsers} label="Online Players" value={snapshot.overview.online_players} />
                <MetricCard detail="Ready to join" icon={faDoorOpen} label="Open Lobbies" value={snapshot.overview.open_lobbies} />
                <MetricCard detail="Starting or underway" icon={faGamepad} label="Active Games" value={snapshot.overview.active_games} />
                <MetricCard detail="Waiting in Quick Match" icon={faClock} label="Queued Players" value={snapshot.overview.queued_players} />
              </div>
            )}
          </section>
        ) : null}

        {route === "/leaderboard" ? (
          <section aria-labelledby="leaderboard-heading">
            <SectionCard description="Top competitors ranked by their current Online rating." heading="Leaderboard" headingId="leaderboard-heading" icon={faGaugeHigh}>
              <DataGrid
                aria-label="Player leaderboard"
                columns={leaderboardColumns}
                contentClassName="min-w-[660px]"
                data={rankedPlayers}
                getRowId={(player) => player.display_name}
                renderEmptyState={() => !snapshot
                  ? error
                    ? <EmptyTableState icon={faTriangleExclamation}>Leaderboard data is unavailable.</EmptyTableState>
                    : <TableLoadingState />
                  : <EmptyTableState icon={faUsersSlash}>No ranked players yet.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </SectionCard>
          </section>
        ) : null}

        {route === "/game-lobbies" ? (
          <section aria-labelledby="lobbies-heading">
            <SectionCard description="Public games that are currently accepting players." heading="Game Lobbies" headingId="lobbies-heading" icon={faDoorOpen}>
              <DataGrid
                aria-label="Open game lobbies"
                columns={lobbyColumns}
                contentClassName="min-w-[760px]"
                data={lobbyRows}
                getRowId={(lobby) => lobby.rowKey}
                renderEmptyState={() => !snapshot
                  ? error
                    ? <EmptyTableState icon={faTriangleExclamation}>Lobby data is unavailable.</EmptyTableState>
                    : <TableLoadingState />
                  : <EmptyTableState icon={faDoorOpen}>No public lobbies are open.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </SectionCard>
          </section>
        ) : null}

        {route === "/online-players" ? (
          <section aria-labelledby="players-heading">
            <SectionCard description="Players currently connected to the Online service." heading="Online Players" headingId="players-heading" icon={faUsers}>
              <DataGrid
                aria-label="Online players"
                columns={playerColumns}
                contentClassName="min-w-[420px]"
                data={snapshot?.online_players || []}
                getRowId={(player) => player.display_name}
                renderEmptyState={() => !snapshot
                  ? error
                    ? <EmptyTableState icon={faTriangleExclamation}>Player data is unavailable.</EmptyTableState>
                    : <TableLoadingState />
                  : <EmptyTableState icon={faUsersSlash}>No players are online.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </SectionCard>
          </section>
        ) : null}

        {route === "/active-games" ? (
          <section aria-labelledby="active-games-heading">
            <SectionCard description="Matches that are starting or already underway." heading="Active Games" headingId="active-games-heading" icon={faGamepad}>
              <DataGrid
                aria-label="Active games"
                columns={activeGameColumns}
                contentClassName="min-w-[620px]"
                data={activeGameRows}
                getRowId={(game) => game.rowKey}
                renderEmptyState={() => !snapshot
                  ? error
                    ? <EmptyTableState icon={faTriangleExclamation}>Active game data is unavailable.</EmptyTableState>
                    : <TableLoadingState />
                  : <EmptyTableState icon={faGamepad}>No public games are active.</EmptyTableState>}
                scrollContainerClassName="overflow-x-auto"
                variant="secondary"
              />
            </SectionCard>
          </section>
        ) : null}

        {route === "/how-to-play" ? <HowToPlayPage /> : null}
      </main>

      <footer className="mx-auto max-w-7xl px-6 pb-8 text-sm text-muted lg:px-8">
        <p className="flex items-center gap-2">
          <FontAwesomeIcon aria-hidden="true" icon={faCircleCheck} />
          GeneralsX Online community activity
        </p>
      </footer>
    </div>
  );
}
