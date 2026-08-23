// Types mirror the console's JSON API (/api/roster). Kept minimal — the
// fields the People view reads.
export interface Membership {
  member?: boolean;
  invitationPending?: boolean;
  role?: string;
  state?: string;
}

export interface Person {
  name: string;
  github: string;
  class?: string;
  live?: boolean;
  state?: string;
  orgs?: Record<string, Membership>;
}

export interface Roster {
  people?: Person[];
}

export async function fetchRoster(signal?: AbortSignal): Promise<Roster> {
  const resp = await fetch("/api/roster", {
    headers: { Accept: "application/json" },
    signal,
  });
  if (!resp.ok) {
    throw new Error(`GET /api/roster: ${resp.status}`);
  }
  return (await resp.json()) as Roster;
}
