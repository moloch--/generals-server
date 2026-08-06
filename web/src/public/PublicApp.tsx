import {DataGrid, type DataGridColumn} from "@heroui-pro/react/data-grid";
import {KPI} from "@heroui-pro/react/kpi";
import {Navbar} from "@heroui-pro/react/navbar";
import type {IconDefinition} from "@fortawesome/fontawesome-svg-core";
import {
  faArrowsRotate,
  faCircleCheck,
  faClock,
  faDoorOpen,
  faGamepad,
  faGaugeHigh,
  faTriangleExclamation,
  faUsers,
  faUsersSlash,
} from "@fortawesome/free-solid-svg-icons";
import {FontAwesomeIcon} from "@fortawesome/react-fontawesome";
import {Alert} from "@heroui/react/alert";
import {Button} from "@heroui/react/button";
import {Card} from "@heroui/react/card";
import {Chip} from "@heroui/react/chip";
import {Skeleton} from "@heroui/react/skeleton";
import {useEffect, useMemo, useState} from "react";

import {
  type ActiveGame,
  fetchPublicSnapshot,
  type LeaderboardEntry,
  type OnlinePlayer,
  type PublicLobby,
  type PublicSnapshot,
} from "./api";

const pollIntervalMilliseconds = 10_000;
const staleAfterMilliseconds = 30_000;
const appIconURL = `${import.meta.env.BASE_URL}generalsx-zh-icon.png`;

const navigationItems = [
  {id: "overview", label: "Overview"},
  {id: "leaderboard", label: "Leaderboard"},
  {id: "game-lobbies", label: "Game Lobbies"},
  {id: "online-players", label: "Online Players"},
  {id: "active-games", label: "Active Games"},
] as const;

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
        <Card.Title id={headingId} render={(props) => <h2 {...props} />}>
          <IconTitle icon={icon}>{heading}</IconTitle>
        </Card.Title>
        <Card.Description>{description}</Card.Description>
      </Card.Header>
      <Card.Content className="px-0 pb-0 pt-4">{children}</Card.Content>
    </Card>
  );
}

export function PublicApp() {
  const [snapshot, setSnapshot] = useState<PublicSnapshot | null>(null);
  const [error, setError] = useState("");
  const [lastSuccessAt, setLastSuccessAt] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  const [refreshKey, setRefreshKey] = useState(0);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [isMenuOpen, setIsMenuOpen] = useState(false);
  const [activeSection, setActiveSection] = useState(() => window.location.hash.slice(1) || "overview");

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 5_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    const sections = navigationItems
      .map(({id}) => document.getElementById(id))
      .filter((section): section is HTMLElement => Boolean(section));
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((entry) => entry.isIntersecting)
          .sort((first, second) => second.intersectionRatio - first.intersectionRatio)[0];
        if (visible?.target.id) {
          setActiveSection(visible.target.id);
        }
      },
      {rootMargin: "-15% 0px -84%", threshold: 0},
    );
    sections.forEach((section) => observer.observe(section));
    return () => observer.disconnect();
  }, [snapshot]);

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
      setIsRefreshing(true);
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
          setIsRefreshing(false);
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
  const activityLabel = !snapshot ? "Connecting" : isStale ? "Update delayed" : isRefreshing ? "Refreshing" : "Live";
  const activityIcon = !snapshot || isRefreshing ? faArrowsRotate : isStale ? faTriangleExclamation : faCircleCheck;

  return (
    <div className="min-h-screen bg-background text-foreground">
      <Navbar
        isMenuOpen={isMenuOpen}
        maxWidth="2xl"
        position="sticky"
        shouldBlockScroll={false}
        onMenuOpenChange={setIsMenuOpen}
      >
        <Navbar.Header>
          <Navbar.MenuToggle className="lg:hidden" />
          <Navbar.Brand>
            <a className="flex items-center gap-3 no-underline" href="#overview" onClick={() => setIsMenuOpen(false)}>
              <img alt="" aria-hidden="true" className="size-9 rounded-xl" src={appIconURL} />
              <span className="font-semibold tracking-tight text-foreground">GeneralsX Online</span>
            </a>
          </Navbar.Brand>
          <Navbar.Spacer />
          <Navbar.Content className="hidden lg:flex">
            {navigationItems.map((item) => (
              <Navbar.Item href={`#${item.id}`} isCurrent={activeSection === item.id} key={item.id}>
                {item.label}
              </Navbar.Item>
            ))}
          </Navbar.Content>
          <Navbar.Spacer className="hidden lg:block" />
          <Navbar.Content>
            <span aria-live="polite">
              <Chip color={isStale || !snapshot ? "warning" : "success"} size="sm" variant="soft">
                <Chip.Label className="flex items-center gap-1.5">
                  <FontAwesomeIcon
                    aria-hidden="true"
                    className={!snapshot || isRefreshing ? "animate-spin motion-reduce:animate-none" : undefined}
                    icon={activityIcon}
                  />
                  {activityLabel}
                </Chip.Label>
              </Chip>
            </span>
          </Navbar.Content>
        </Navbar.Header>
        <Navbar.Menu>
          {navigationItems.map((item) => (
            <Navbar.MenuItem
              href={`#${item.id}`}
              isCurrent={activeSection === item.id}
              key={item.id}
              onClick={() => setIsMenuOpen(false)}
            >
              {item.label}
            </Navbar.MenuItem>
          ))}
        </Navbar.Menu>
      </Navbar>

      <main className="mx-auto max-w-7xl space-y-10 px-6 py-8 lg:px-8">
        <section aria-labelledby="overview-heading" className="scroll-mt-24 space-y-5" id="overview">
          <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h1 className="flex items-center gap-2.5 text-2xl font-semibold tracking-tight" id="overview-heading">
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

        <section aria-labelledby="leaderboard-heading" className="scroll-mt-24" id="leaderboard">
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

        <section aria-labelledby="lobbies-heading" className="scroll-mt-24" id="game-lobbies">
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

        <section aria-labelledby="players-heading" className="scroll-mt-24" id="online-players">
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

        <section aria-labelledby="active-games-heading" className="scroll-mt-24" id="active-games">
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
