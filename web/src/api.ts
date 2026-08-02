export interface HubStats {
  online_players: number;
  open_games: number;
  active_games: number;
  queued_players: number;
}

export interface RelayStats {
  datagrams_in: string;
  datagrams_out: string;
  bytes_in: string;
  bytes_out: string;
  dropped_malformed: string;
  dropped_auth: string;
  dropped_rate_limit: string;
  dropped_no_endpoint: string;
  buffered_until_bind: string;
  active_games: number;
}

export interface Overview {
  status: string;
  protocol: number;
  started_at: string;
  uptime_seconds: string;
  profile_count: string;
  hub: HubStats;
  relay: RelayStats;
}

export interface Profile {
  user_id: string;
  username: string;
  display_name: string;
  created_at: string;
  wins: string;
  losses: string;
  disconnects: string;
  games: string;
  rating: string;
}

export interface ProfilePage {
  profiles: Profile[];
  total: string;
  limit: number;
  offset: number;
}

export interface Session {
  user_id: string;
  username?: string;
  display_name: string;
  guest: boolean;
  created_at: string;
  status: string;
  room_id?: string;
  game_id?: string;
  quickmatch_queued: boolean;
}

export interface GameMember {
  user_id: string;
  display_name: string;
  host: boolean;
  ready: boolean;
  slot: number;
}

export interface Game {
  game_id: string;
  name: string;
  map?: string;
  host_name: string;
  players: number;
  max_players: number;
  has_password: boolean;
  state: string;
  listed: boolean;
  product: string;
  compatibility_version: number;
  ini_crc: number;
  members: GameMember[];
}

interface DataEnvelope<T> {
  data: T;
}

interface ErrorEnvelope {
  error?: {
    code?: string;
    message?: string;
  };
}

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

export class AdminApi {
  constructor(private readonly token: string) {}

  overview(signal?: AbortSignal): Promise<Overview> {
    return this.request<Overview>("/api/admin/v1/overview", {signal});
  }

  profiles(query: string, limit: number, offset: number, signal?: AbortSignal): Promise<ProfilePage> {
    const parameters = new URLSearchParams({
      limit: String(limit),
      offset: String(offset),
    });
    if (query.trim()) {
      parameters.set("query", query.trim());
    }
    return this.request<ProfilePage>(`/api/admin/v1/profiles?${parameters}`, {signal});
  }

  async sessions(signal?: AbortSignal): Promise<Session[]> {
    const result = await this.request<{sessions: Session[]}>("/api/admin/v1/sessions", {signal});
    return result.sessions;
  }

  async games(signal?: AbortSignal): Promise<Game[]> {
    const result = await this.request<{games: Game[]}>("/api/admin/v1/games", {signal});
    return result.games;
  }

  disconnect(userID: string): Promise<void> {
    return this.request<void>(`/api/admin/v1/sessions/${encodeURIComponent(userID)}`, {method: "DELETE"});
  }

  closeGame(gameID: string): Promise<void> {
    return this.request<void>(`/api/admin/v1/games/${encodeURIComponent(gameID)}`, {method: "DELETE"});
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(path, {
      ...init,
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${this.token}`,
        ...init.headers,
      },
    });

    if (response.status === 204) {
      return undefined as T;
    }

    const body = (await response.json().catch(() => null)) as DataEnvelope<T> | ErrorEnvelope | null;
    if (!response.ok) {
      const message = body && "error" in body ? body.error?.message : undefined;
      throw new ApiError(response.status, message || `The server returned HTTP ${response.status}.`);
    }
    if (!body || !("data" in body)) {
      throw new ApiError(response.status, "The server returned an invalid response.");
    }
    return body.data;
  }
}
