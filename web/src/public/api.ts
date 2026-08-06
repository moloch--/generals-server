export interface PublicOverview {
  online_players: number;
  open_lobbies: number;
  active_games: number;
  queued_players: number;
}

export interface LeaderboardEntry {
  display_name: string;
  wins: string;
  losses: string;
  games: string;
  rating: string;
}

export interface OnlinePlayer {
  display_name: string;
  status: string;
}

export interface PublicLobby {
  name: string;
  map: string;
  host_name: string;
  players: number;
  max_players: number;
  has_password: boolean;
  product: string;
}

export interface ActiveGame {
  name: string;
  map: string;
  players: number;
  max_players: number;
  product: string;
  state: string;
}

export interface PublicSnapshot {
  generated_at: string;
  overview: PublicOverview;
  leaderboard: LeaderboardEntry[];
  online_players: OnlinePlayer[];
  lobbies: PublicLobby[];
  active_games: ActiveGame[];
}

interface DataEnvelope {
  data: unknown;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function hasString(record: Record<string, unknown>, key: string): boolean {
  return typeof record[key] === "string";
}

function hasNumber(record: Record<string, unknown>, key: string): boolean {
  const value = record[key];
  return typeof value === "number" && Number.isInteger(value) && value >= 0;
}

function isOverview(value: unknown): value is PublicOverview {
  if (!isRecord(value)) {
    return false;
  }
  return hasNumber(value, "online_players")
    && hasNumber(value, "open_lobbies")
    && hasNumber(value, "active_games")
    && hasNumber(value, "queued_players");
}

function isLeaderboardEntry(value: unknown): value is LeaderboardEntry {
  if (!isRecord(value)) {
    return false;
  }
  return hasString(value, "display_name")
    && hasString(value, "wins")
    && hasString(value, "losses")
    && hasString(value, "games")
    && hasString(value, "rating");
}

function isOnlinePlayer(value: unknown): value is OnlinePlayer {
  return isRecord(value) && hasString(value, "display_name") && hasString(value, "status");
}

function isLobby(value: unknown): value is PublicLobby {
  if (!isRecord(value)) {
    return false;
  }
  return hasString(value, "name")
    && hasString(value, "map")
    && hasString(value, "host_name")
    && hasNumber(value, "players")
    && hasNumber(value, "max_players")
    && typeof value.has_password === "boolean"
    && hasString(value, "product");
}

function isActiveGame(value: unknown): value is ActiveGame {
  if (!isRecord(value)) {
    return false;
  }
  return hasString(value, "name")
    && hasString(value, "map")
    && hasNumber(value, "players")
    && hasNumber(value, "max_players")
    && hasString(value, "product")
    && hasString(value, "state");
}

function isArrayOf<T>(value: unknown, predicate: (item: unknown) => item is T): value is T[] {
  return Array.isArray(value) && value.every(predicate);
}

function isSnapshot(value: unknown): value is PublicSnapshot {
  if (!isRecord(value)) {
    return false;
  }
  return hasString(value, "generated_at")
    && isOverview(value.overview)
    && isArrayOf(value.leaderboard, isLeaderboardEntry)
    && isArrayOf(value.online_players, isOnlinePlayer)
    && isArrayOf(value.lobbies, isLobby)
    && isArrayOf(value.active_games, isActiveGame);
}

function responseMessage(status: number, body: unknown): string {
  if (isRecord(body) && isRecord(body.error) && typeof body.error.message === "string") {
    return body.error.message;
  }
  return `The server returned HTTP ${status}.`;
}

export async function fetchPublicSnapshot(signal?: AbortSignal): Promise<PublicSnapshot> {
  const response = await fetch("/api/public/v1/snapshot", {
    cache: "no-store",
    credentials: "omit",
    headers: {Accept: "application/json"},
    method: "GET",
    signal,
  });
  const body: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    throw new Error(responseMessage(response.status, body));
  }
  if (!isRecord(body) || !("data" in body)) {
    throw new Error("The server returned an invalid response envelope.");
  }
  const envelope = body as unknown as DataEnvelope;
  if (!isSnapshot(envelope.data)) {
    throw new Error("The server returned an invalid public snapshot.");
  }
  return envelope.data;
}
