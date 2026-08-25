import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { RosterService } from "./gen/roster/v1/roster_pb.js";

// rosterClient is the typed ConnectRPC (Connect-Web) client. It talks to the
// same server as the fetch() calls below, but over the generated contract —
// the first endpoint migrated off the ad-hoc /api/* JSON, with more views to
// follow.
const rosterClient = createClient(RosterService, createConnectTransport({ baseUrl: "/" }));

// fetchVersion returns the running build's identity via ConnectRPC
// (replacing a JSON GET /api/version).
export async function fetchVersion(signal?: AbortSignal): Promise<{ version: string; commit: string }> {
  const resp = await rosterClient.getVersion({}, { signal });

  return { version: resp.version, commit: resp.commit };
}

// fetchMe returns the signed-in caller (name, email, role) as the gateway
// forwards it, for the header's user badge.
export interface Me { name: string; email: string; role: string }

export async function fetchMe(signal?: AbortSignal): Promise<Me> {
  const resp = await rosterClient.getMe({}, { signal });

  return { name: resp.name, email: resp.email, role: resp.role };
}

// stageOrg stages an operator-added organization in the config store (name +
// one seed team, optional minimum-owners). Operator-only; the server rejects a
// viewer. On success the org appears in Settings with a "Create GitHub App"
// link.
export interface StageOrgInput {
  name: string;
  minAdmins?: number;
  team: string;
  groups?: string[];
  members?: string[];
}

export async function stageOrg(input: StageOrgInput): Promise<void> {
  await rosterClient.stageOrg({
    name: input.name,
    minAdmins: input.minAdmins ?? 0,
    team: input.team,
    groups: input.groups ?? [],
    members: input.members ?? [],
  });
}

// Person (mapping) mutations — Approve/Add, Adopt, Edit, Remove. Operator-only.
export interface PersonInput {
  name: string;
  github: string;
  emails?: string[];
  k8s?: string;
  class?: string;
  pinned?: string[];
}

export async function putPerson(p: PersonInput): Promise<void> {
  await rosterClient.putPerson({
    name: p.name, github: p.github, emails: p.emails ?? [], k8s: p.k8s ?? "", class: p.class ?? "", pinned: p.pinned ?? [],
  });
}

export async function deletePerson(name: string): Promise<void> {
  await rosterClient.deletePerson({ name });
}

// getPerson reads one raw mapping entry so the Edit form can prefill every
// field the joined Person omits (emails, pinned).
export async function getPerson(name: string): Promise<PersonInput> {
  const p = await rosterClient.getPerson({ name });

  return {
    name: p.name,
    github: p.github,
    emails: p.emails,
    k8s: p.k8s || undefined,
    class: p.class || undefined,
    pinned: p.pinned,
  };
}

// Operational mutations (operator-only; the server rejects a viewer).
export async function addDirectory(input: { name: string; domains: string[]; endpoint: string; probeGroup?: string }): Promise<void> {
  await rosterClient.addDirectory({ name: input.name, domains: input.domains, endpoint: input.endpoint, probeGroup: input.probeGroup ?? "" });
}

export async function deleteDirectory(name: string): Promise<void> {
  await rosterClient.deleteDirectory({ name });
}

export async function setPaused(org: string, paused: boolean): Promise<void> {
  await rosterClient.setPaused({ org, paused });
}

export async function runReconcile(): Promise<void> {
  await rosterClient.runReconcile({});
}

// Types mirror the console's JSON API (/api/roster). Kept minimal — the
// fields the People view reads.
export interface Membership {
  member?: boolean;
  invitationPending?: boolean;
  role?: string;
  state?: string;
  live?: boolean;
  teams?: string[];
  desiredTeams?: string[];
}

// DirectoryIdentity is how one directory (IdP source) knows a person.
export interface DirectoryIdentity {
  email?: string;
  live?: boolean;
}

export interface Person {
  name: string;
  github: string;
  class?: string;
  live?: boolean;
  state?: string;
  orgs?: Record<string, Membership>;
  email?: string;
  sources?: string[];
  expectedSources?: string[];
  noTeam?: boolean;
  directories?: Record<string, DirectoryIdentity>;
}

// Candidate is an awaiting-approval worklist row. NEW carries a name (login to
// be supplied); UNKNOWN carries a github login (name to be supplied).
export interface Candidate {
  kind: "new" | "unknown" | string;
  name: string;
  github: string;
  org?: string;
  detail?: string;
  // IdP identity behind a NEW candidate — how the join found them.
  email?: string;
  sources?: string[];
  directories?: Record<string, DirectoryIdentity>;
}

export interface Roster {
  people?: Person[];
  candidates?: Candidate[];
}

// fetchRoster reads the joined roster over ConnectRPC (the JSON /api/roster
// stays for the cross-repo puller). Empty scalars → undefined, matching the
// omitempty JSON the People view was written against.
export async function fetchRoster(signal?: AbortSignal): Promise<Roster> {
  const resp = await rosterClient.getRoster({}, { signal });

  return {
    people: resp.people.map((p) => ({
      name: p.name,
      github: p.github,
      class: p.class || undefined,
      live: p.live,
      state: p.state || undefined,
      orgs: Object.fromEntries(
        Object.entries(p.orgs).map(([org, m]) => [org, {
          member: m.member,
          invitationPending: m.invitationPending,
          role: m.role || undefined,
          state: m.state || undefined,
          live: m.live,
          teams: m.teams,
          desiredTeams: m.desiredTeams,
        }]),
      ),
      email: p.email || undefined,
      sources: p.sources,
      expectedSources: p.expectedSources,
      noTeam: p.noTeam,
      directories: Object.fromEntries(
        Object.entries(p.directories).map(([src, d]) => [src, { email: d.email || undefined, live: d.live }]),
      ),
    })),
    candidates: resp.candidates.map((c) => ({
      kind: c.kind, name: c.name, github: c.github, org: c.org || undefined, detail: c.detail || undefined,
      email: c.email || undefined, sources: c.sources,
      directories: Object.fromEntries(
        Object.entries(c.directories).map(([src, d]) => [src, { email: d.email || undefined, live: d.live }]),
      ),
    })),
  };
}

// ReconcileChange is one concrete action behind the counts, so the Status view
// can expand "+2 invite" into who and which team.
export interface ReconcileChange {
  verb: string;
  login: string;
  team?: string;
}

export interface ReconcileStatus {
  org: string;
  enabled?: boolean;
  paused?: boolean;
  at?: string;
  actions?: number;
  adds?: number;
  removes?: number;
  roleChanges?: number;
  teamChanges?: number;
  details?: ReconcileChange[];
  applied?: boolean;
  held?: boolean;
  reason?: string;
  error?: string;
}

// fetchStatus reads reconcile status over ConnectRPC. The gateway injects the
// operator's bearer token onto the proxied request (as it did for the JSON
// endpoint), which the broker authorizes.
export async function fetchStatus(signal?: AbortSignal): Promise<ReconcileStatus[]> {
  const resp = await rosterClient.getStatus({}, { signal });

  return resp.statuses.map((s) => ({
    org: s.org,
    enabled: s.enabled,
    paused: s.paused,
    at: s.at || undefined,
    actions: s.actions,
    adds: s.adds,
    removes: s.removes,
    roleChanges: s.roleChanges,
    teamChanges: s.teamChanges,
    details: s.details.map((d) => ({ verb: d.verb, login: d.login, team: d.team || undefined })),
    applied: s.applied,
    held: s.held,
    reason: s.reason || undefined,
    error: s.error || undefined,
  }));
}

export async function setReconcileEnabled(org: string, enabled: boolean): Promise<void> {
  await rosterClient.setReconcileEnabled({ org, enabled });
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

// fetchAudit reads audit records over ConnectRPC (the JSON /api/audit stays for
// tooling).
export async function fetchAudit(signal?: AbortSignal): Promise<AuditRecord[]> {
  const resp = await rosterClient.getAudit({}, { signal });

  return resp.records.map((r) => ({
    at: r.at,
    org: r.org,
    kind: r.kind || undefined,
    confirmed: r.confirmed,
    actor: r.actor || undefined,
    actorEmail: r.actorEmail || undefined,
    adding: r.adding.length ? r.adding : undefined,
    removing: r.removing.length ? r.removing : undefined,
    changes: r.changes.length
      ? r.changes.map((c) => ({ verb: c.verb, subject: c.subject, team: c.team || undefined, detail: c.detail || undefined }))
      : undefined,
    error: r.error || undefined,
  }));
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
export interface SettingsOrg { name: string; company: string; minAdmins: number; reconcileEnabled: boolean; provenance?: string; teams?: SettingsTeam[] }
export interface SettingsSource { name: string; domains?: string[]; endpoint?: string; probeGroup?: string }
export interface Settings { sources?: SettingsSource[]; storeSources?: SettingsSource[]; orgs?: SettingsOrg[]; storeOrgs?: SettingsOrg[] }

// fetchSettings now reads over ConnectRPC (was GET /api/settings), mapping the
// typed response onto the view's Settings shape. Empty scalars become undefined
// to match the previous omitempty JSON the view was written against.
export async function fetchSettings(signal?: AbortSignal): Promise<Settings> {
  const resp = await rosterClient.getSettings({}, { signal });

  const source = (s: { name: string; domains: string[]; endpoint: string; probeGroup: string }): SettingsSource => ({
    name: s.name,
    domains: s.domains.length ? s.domains : undefined,
    endpoint: s.endpoint || undefined,
    probeGroup: s.probeGroup || undefined,
  });

  const org = (o: { name: string; company: string; minAdmins: number; reconcileEnabled: boolean; provenance: string; teams: { name: string; groups: string[]; members: string[]; pinned: boolean }[] }): SettingsOrg => ({
    name: o.name,
    company: o.company,
    minAdmins: o.minAdmins,
    reconcileEnabled: o.reconcileEnabled,
    provenance: o.provenance || undefined,
    teams: o.teams.map((t) => ({
      name: t.name,
      groups: t.groups.length ? t.groups : undefined,
      members: t.members.length ? t.members : undefined,
      pinned: t.pinned || undefined,
    })),
  });

  return {
    sources: resp.sources.map(source),
    storeSources: resp.storeSources.map(source),
    orgs: resp.orgs.map(org),
    storeOrgs: resp.storeOrgs.map(org),
  };
}
