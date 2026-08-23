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

export interface ReconcileStatus {
  org: string;
  enabled?: boolean;
  paused?: boolean;
  at?: string;
  actions?: number;
  applied?: boolean;
  held?: boolean;
  reason?: string;
  error?: string;
}

export async function fetchStatus(signal?: AbortSignal): Promise<ReconcileStatus[]> {
  const resp = await fetch("/api/status", { headers: { Accept: "application/json" }, signal });
  if (!resp.ok) throw new Error(`GET /api/status: ${resp.status}`);
  return (await resp.json()) as ReconcileStatus[];
}

export interface AuditChange {
  verb: string;
  subject: string;
  team?: string;
  detail?: string;
}

export interface AuditRecord {
  at: string;
  org: string;
  kind?: string;
  confirmed?: boolean;
  actor?: string;
  actorEmail?: string;
  adding?: string[];
  removing?: string[];
  changes?: AuditChange[];
  error?: string;
}

export async function fetchAudit(signal?: AbortSignal): Promise<AuditRecord[]> {
  const resp = await fetch("/api/audit", { headers: { Accept: "application/json" }, signal });
  if (!resp.ok) throw new Error(`GET /api/audit: ${resp.status}`);
  return ((await resp.json()) as AuditRecord[]) ?? [];
}

export interface Change {
  at: string;
  org: string;
  kind?: string;
  verb: string;
  subject: string;
  actor: string;
  detail?: string;
}

export function flattenAudit(records: AuditRecord[]): Change[] {
  const out: Change[] = [];
  for (const r of records) {
    if (r.confirmed === false) continue;
    const actor = r.actorEmail || r.actor || "operator";
    const at = r.at?.slice(0, 16).replace("T", " ") ?? "";
    if (r.changes && r.changes.length) {
      for (const c of r.changes) {
        const detail = [c.team, c.detail].filter(Boolean).join(" ");
        out.push({ at, org: r.org, kind: r.kind, verb: c.verb, subject: c.subject, actor, detail });
      }
    } else {
      for (const s of r.adding ?? []) out.push({ at, org: r.org, kind: r.kind, verb: "added", subject: s, actor });
      for (const s of r.removing ?? []) out.push({ at, org: r.org, kind: r.kind, verb: "removed", subject: s, actor });
    }
    if (r.error) out.push({ at, org: r.org, kind: r.kind, verb: "failed", subject: "", actor, detail: r.error });
  }
  return out.sort((a, b) => (a.at < b.at ? 1 : -1));
}

export interface SettingsTeam { name: string; groups?: string[]; members?: string[]; pinned?: boolean }
export interface SettingsOrg { name: string; company: string; minAdmins: number; reconcileEnabled: boolean; teams?: SettingsTeam[] }
export interface SettingsSource { name: string; domains?: string[]; endpoint?: string; probeGroup?: string }
export interface Settings { sources?: SettingsSource[]; orgs?: SettingsOrg[] }

export async function fetchSettings(signal?: AbortSignal): Promise<Settings> {
  const resp = await fetch("/api/settings", { headers: { Accept: "application/json" }, signal });
  if (!resp.ok) throw new Error(`GET /api/settings: ${resp.status}`);
  return (await resp.json()) as Settings;
}
