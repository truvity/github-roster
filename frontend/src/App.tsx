import { useEffect, useMemo, useState, type FormEvent } from "react";
import {
  fetchRoster, fetchStatus, fetchAudit, flattenAudit,
  fetchSettings, fetchVersion, fetchMe, stageOrg,
  addDirectory, deleteDirectory, setPaused, runReconcile, putPerson, deletePerson, setReconcileEnabled,
  type Person, type ReconcileStatus, type Change, type Settings, type SettingsOrg, type Candidate,
} from "./api";

type Tab = "people" | "status" | "history" | "settings";

function badge(state?: string): { label: string; cls: string } {
  switch (state) {
    case "synced": return { label: "synced", cls: "ok" };
    case "pending": return { label: "pending", cls: "warn" };
    case "invited": return { label: "invited", cls: "warn" };
    case "leaving": return { label: "leaving", cls: "danger" };
    case "left": return { label: "left", cls: "muted" };
    case "bot": return { label: "bot", cls: "muted" };
    default: return { label: "—", cls: "muted" };
  }
}

// useAsync runs loader and tracks {data,error}. It re-runs whenever a value in
// deps changes (default: once), so a mutation can bump a counter to refetch.
function useAsync<T>(loader: (s: AbortSignal) => Promise<T>, deps: unknown[] = []): { data: T | null; error: string } {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const ac = new AbortController();
    loader(ac.signal).then(setData).catch((e) => { if (e.name !== "AbortError") setError(String(e)); });
    return () => ac.abort();
  }, deps); // eslint-disable-line react-hooks/exhaustive-deps
  return { data, error };
}

type Preset = "needs" | "active" | "left" | "bots" | "all";

function peopleMatches(p: Person, preset: Preset, q: string): boolean {
  if (q && !`${p.name} ${p.github}`.toLowerCase().includes(q.toLowerCase())) return false;
  switch (preset) {
    case "needs": return ["pending", "invited", "leaving"].includes(p.state ?? "");
    case "active": return ["synced", "pending", "invited"].includes(p.state ?? "");
    case "left": return p.state === "left";
    case "bots": return p.class === "bot";
    default: return true;
  }
}

// PersonForm adds (or re-approves) a roster mapping entry — the operator fills
// the data and blesses the person in one action (approvedBy/At are stamped
// server-side). Editing an existing name replaces its entry.
function PersonForm({ onSaved, onCancel }: { onSaved: () => void; onCancel: () => void }) {
  const [name, setName] = useState("");
  const [github, setGithub] = useState("");
  const [k8s, setK8s] = useState("");
  const [cls, setCls] = useState("employee");
  const [emails, setEmails] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      await putPerson({ name: name.trim(), github: github.trim(), k8s: k8s.trim(), class: cls, emails: emails.split(",").map((x) => x.trim()).filter(Boolean) });
      onSaved();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} style={{ margin: "12px 0", padding: 12, border: "1px solid var(--border, #ddd)", borderRadius: 8, display: "flex", gap: 10, flexWrap: "wrap", alignItems: "flex-end" }}>
      <label>Name<br /><input value={name} onChange={(e) => setName(e.target.value)} placeholder="Ada Lovelace" required /></label>
      <label>GitHub login<br /><input value={github} onChange={(e) => setGithub(e.target.value)} placeholder="ada" required /></label>
      <label>Namespace (k8s)<br /><input value={k8s} onChange={(e) => setK8s(e.target.value)} placeholder="alovelace" /></label>
      <label>Class<br /><select value={cls} onChange={(e) => setCls(e.target.value)}><option value="employee">employee</option><option value="bot">bot</option></select></label>
      <label>Emails (comma-separated)<br /><input value={emails} onChange={(e) => setEmails(e.target.value)} style={{ width: 220 }} placeholder="ada@acme.com" /></label>
      <button type="submit" disabled={busy}>{busy ? "Saving…" : "Add person"}</button>
      <button type="button" className="chip" onClick={onCancel}>Cancel</button>
      {err && <span className="banner">{err}</span>}
    </form>
  );
}

// CandidateRow is one awaiting-approval worklist row: editable name + login
// (each prefilled from the side the join knows), approved with one click.
function CandidateRow({ c, onApproved }: { c: Candidate; onApproved: () => void }) {
  const [name, setName] = useState(c.name);
  const [github, setGithub] = useState(c.github);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const approve = async () => {
    setErr(""); setBusy(true);
    try { await putPerson({ name: name.trim(), github: github.trim(), k8s: name.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-"), class: "employee" }); onApproved(); }
    catch (e) { setErr(e instanceof Error ? e.message : String(e)); setBusy(false); }
  };
  const ready = name.trim() !== "" && github.trim() !== "";
  return (
    <tr>
      <td><span className={`badge ${c.kind === "unknown" ? "danger" : "warn"}`}>{c.kind === "unknown" ? "unknown" : "new"}</span></td>
      <td><input value={name} onChange={(e) => setName(e.target.value)} placeholder="Full Name" style={{ width: 160 }} /></td>
      <td><input className="mono" value={github} onChange={(e) => setGithub(e.target.value)} placeholder="login" style={{ width: 140 }} /></td>
      <td className="muted">{c.org || ""} {c.detail}</td>
      <td><button className="chip" disabled={!ready || busy} onClick={approve} title={c.kind === "unknown" ? "Adopt this member" : "Approve & invite"}>{busy ? "…" : c.kind === "unknown" ? "Adopt" : "Approve"}</button>{err && <span className="banner">{err}</span>}</td>
    </tr>
  );
}

// A candidate (awaiting approval) belongs to the operator's worklist, so it
// shows under "Needs me" and "All", and matches the search box.
function candidateMatches(c: Candidate, preset: Preset, q: string): boolean {
  if (q && !`${c.name} ${c.github}`.toLowerCase().includes(q.toLowerCase())) return false;
  return preset === "needs" || preset === "all";
}

// orgCell renders a person's per-org membership as compact badges in one cell,
// so members and candidates share the same columns.
function orgCell(p: Person) {
  const entries = Object.entries(p.orgs ?? {}).filter(([, m]) => m.state);
  if (entries.length === 0) return <span className="muted">—</span>;
  return <>{entries.map(([o, m]) => { const b = badge(m.state); return <span key={o} className={`badge ${b.cls}`} style={{ marginRight: 4 }}>{o}: {b.label}</span>; })}</>;
}

function PeopleView() {
  const [reload, setReload] = useState(0);
  const [actErr, setActErr] = useState("");
  const [preset, setPreset] = useState<Preset>("needs");
  const [q, setQ] = useState("");
  const [adding, setAdding] = useState(false);
  const { data, error } = useAsync((s) => fetchRoster(s), [reload]);
  const refresh = () => setReload((r) => r + 1);
  const remove = async (name: string) => {
    setActErr("");
    try { await deletePerson(name); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); }
  };
  const people = data?.people ?? null;
  const candidates = data?.candidates ?? [];
  const shownCandidates = useMemo(() => candidates.filter((c) => candidateMatches(c, preset, q)), [candidates, preset, q]);
  const rows = useMemo(
    () => (people ?? []).filter((p) => peopleMatches(p, preset, q)).sort((a, b) => a.name.localeCompare(b.name)),
    [people, preset, q],
  );
  if (error) return <div className="banner">{error}</div>;
  if (people === null) return <p className="muted">Loading…</p>;
  const needs = candidates.length;
  const presets: [Preset, string][] = [["needs", needs ? `Needs me (${needs})` : "Needs me"], ["active", "Active"], ["left", "Left"], ["bots", "Bots"], ["all", "All"]];
  const empty = shownCandidates.length === 0 && rows.length === 0;
  return (
    <>
      <div className="presets" style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        {presets.map(([k, l]) => <button key={k} className={preset === k ? "chip cur" : "chip"} onClick={() => setPreset(k)}>{l}</button>)}
        <input placeholder="name or login…" value={q} onChange={(e) => setQ(e.target.value)} />
        <button style={{ marginLeft: "auto" }} onClick={() => setAdding((a) => !a)}>{adding ? "Close" : "+ Add person"}</button>
      </div>
      {actErr && <div className="banner">{actErr}</div>}
      {adding && <PersonForm onSaved={() => { setAdding(false); refresh(); }} onCancel={() => setAdding(false)} />}
      <table>
        <thead><tr><th>State</th><th>Name</th><th>GitHub</th><th>Organizations</th><th></th></tr></thead>
        <tbody>
          {shownCandidates.map((c, i) => <CandidateRow key={`c-${c.kind}-${c.name}-${c.github}-${i}`} c={c} onApproved={refresh} />)}
          {rows.map((p) => {
            const b = badge(p.state);
            return (
              <tr key={p.name}>
                <td><span className={`badge ${b.cls}`}>{b.label}</span></td>
                <td>{p.name}</td>
                <td className="mono">{p.github || "—"}</td>
                <td>{orgCell(p)}</td>
                <td><button className="chip" onClick={() => remove(p.name)} title="Remove from the roster">Remove</button></td>
              </tr>
            );
          })}
          {empty && <tr><td colSpan={5} className="muted">Nobody matches this filter.</td></tr>}
        </tbody>
      </table>
    </>
  );
}

function StatusView() {
  const [reload, setReload] = useState(0);
  const { data, error } = useAsync(fetchStatus, [reload]);
  const [busy, setBusy] = useState("");
  const [actErr, setActErr] = useState("");
  const refresh = () => setReload((r) => r + 1);
  const act = async (key: string, fn: () => Promise<void>) => {
    setActErr(""); setBusy(key);
    try { await fn(); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(""); }
  };
  if (error) return <div className="banner">{error}</div>;
  if (data === null) return <p className="muted">Loading…</p>;
  const pending = (s: ReconcileStatus) => {
    if (!s.actions) return <span className="muted">in sync</span>;
    return (
      <span style={{ display: "inline-flex", gap: 4, flexWrap: "wrap" }}>
        {s.adds ? <span className="badge ok">+{s.adds} invite</span> : null}
        {s.removes ? <span className="badge danger">−{s.removes} remove</span> : null}
        {s.roleChanges ? <span className="badge warn">{s.roleChanges} role</span> : null}
        {s.teamChanges ? <span className="badge muted">{s.teamChanges} team</span> : null}
      </span>
    );
  };
  const outcome = (s: ReconcileStatus) => {
    if (s.error) return <span className="badge danger">failed</span>;
    if (s.held) return <><span className="badge warn">held</span> <span className="muted">{s.reason}</span></>;
    if (s.applied) return <span className="badge ok">applied</span>;
    if (!s.actions) return <span className="badge ok">in sync</span>;
    return s.enabled ? <span className="muted">applying next tick</span> : <span className="badge warn">would apply on enable</span>;
  };
  const toggleEnabled = (s: ReconcileStatus) => {
    if (!s.enabled && !window.confirm(`Enable the reconcile loop for ${s.org}? It will start applying the pending changes to GitHub.`)) return;
    act(`enable-${s.org}`, () => setReconcileEnabled(s.org, !s.enabled));
  };
  return (
    <>
      <div className="presets">
        <button className="chip" disabled={busy !== ""} onClick={() => act("run", runReconcile)}>{busy === "run" ? "Reconciling…" : "Sync now"}</button>
      </div>
      {actErr && <div className="banner">{actErr}</div>}
      <table>
        <thead><tr><th>Organization</th><th>Loop</th><th>Pending</th><th>Last run</th><th>Outcome</th><th>Controls</th></tr></thead>
        <tbody>
          {data.map((s) => (
            <tr key={s.org}>
              <td>{s.org}</td>
              <td>{s.enabled ? <span className="badge ok">enabled</span> : <span className="badge warn">disabled</span>}{s.paused && <> <span className="badge danger">paused</span></>}</td>
              <td>{pending(s)}</td>
              <td className="muted">{s.at ? s.at.slice(0, 16).replace("T", " ") : "—"}</td>
              <td>{outcome(s)}</td>
              <td style={{ display: "flex", gap: 6 }}>
                <button className="chip" disabled={busy !== ""} onClick={() => toggleEnabled(s)} title={s.enabled ? "Stop the reconcile loop" : "Start the reconcile loop (applies changes)"}>
                  {s.enabled ? "Disable" : "Enable"}
                </button>
                {s.enabled && (
                  <button className="chip" disabled={busy !== ""} onClick={() => act(`pause-${s.org}`, () => setPaused(s.org, !s.paused))} title="Temporarily halt without disabling">
                    {s.paused ? "Resume" : "Pause"}
                  </button>
                )}
              </td>
            </tr>
          ))}
          {data.length === 0 && <tr><td colSpan={6} className="muted">No reconcile status yet.</td></tr>}
        </tbody>
      </table>
    </>
  );
}

function verbBadge(v: string): { label: string; cls: string } {
  switch (v) {
    case "added": case "team-added": return { label: v, cls: "ok" };
    case "removed": case "team-removed": return { label: v, cls: "danger" };
    case "failed": return { label: "failed", cls: "danger" };
    default: return { label: v, cls: "muted" };
  }
}

function HistoryView() {
  const { data, error } = useAsync((s) => fetchAudit(s).then(flattenAudit));
  const [q, setQ] = useState("");
  const rows = useMemo(
    () => (data ?? []).filter((c) => !q || `${c.subject} ${c.actor}`.toLowerCase().includes(q.toLowerCase())),
    [data, q],
  );
  if (error) return <div className="banner">{error}</div>;
  if (data === null) return <p className="muted">Loading…</p>;
  return (
    <>
      <div className="presets"><input placeholder="login or actor…" value={q} onChange={(e) => setQ(e.target.value)} /></div>
      <table>
        <thead><tr><th>When</th><th>Org</th><th>Kind</th><th>Change</th><th>Who</th><th>By</th></tr></thead>
        <tbody>
          {rows.map((c: Change, i) => {
            const b = verbBadge(c.verb);
            return (
              <tr key={i}>
                <td className="muted">{c.at}</td><td>{c.org}</td>
                <td><span className="badge muted">{c.kind ?? "—"}</span></td>
                <td><span className={`badge ${b.cls}`}>{b.label}</span> {c.detail && <span className="muted">{c.detail}</span>}</td>
                <td className="mono">{c.subject || "—"}</td><td className="muted">{c.actor}</td>
              </tr>
            );
          })}
          {rows.length === 0 && <tr><td colSpan={6} className="muted">No changes match.</td></tr>}
        </tbody>
      </table>
    </>
  );
}

// OrgSection renders one organization and its teams; `staged` marks a
// store-added org (present in the config store, born disabled, not yet run).
function OrgSection({ org: o, staged }: { org: SettingsOrg; staged?: boolean }) {
  return (
    <section style={{ marginTop: 16 }}>
      <h3>
        {o.name}{" "}
        {staged
          ? <span className="badge warn">{o.provenance === "roster" ? "created by roster" : "added manually"} (staged)</span>
          : <span className="badge muted">home: {o.company}</span>}{" "}
        {o.reconcileEnabled ? <span className="badge ok">loop enabled</span> : <span className="badge warn">loop disabled</span>}
      </h3>
      <table>
        <thead><tr><th>Team</th><th>Membership from</th></tr></thead>
        <tbody>
          {(o.teams ?? []).map((t) => (
            <tr key={t.name}>
              <td>{t.name}</td>
              <td className="muted">{t.pinned ? "pinned (operator-edited)" : [...(t.groups ?? []), ...(t.members ?? []).map((m) => `+${m}`)].join(", ")}</td>
            </tr>
          ))}
          {(o.teams ?? []).length === 0 && <tr><td colSpan={2} className="muted">No teams.</td></tr>}
        </tbody>
      </table>
      {staged && (
        <p>
          <a href={`/settings/orgs/create-app?org=${encodeURIComponent(o.name)}`}>Create GitHub App →</a>{" "}
          <span className="muted">redirects to GitHub; the Org Owner creates and installs it, then roster stores the credentials</span>
        </p>
      )}
    </section>
  );
}

// StageOrgForm stages a new organization in the config store (operator-only;
// the server rejects a viewer). On success it calls onStaged to refresh the
// list, where the org then shows a "Create GitHub App" link.
function StageOrgForm({ onStaged }: { onStaged: () => void }) {
  const [name, setName] = useState("");
  const [team, setTeam] = useState("");
  const [groups, setGroups] = useState("");
  const [members, setMembers] = useState("");
  const [minAdmins, setMinAdmins] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const csv = (s: string) => s.split(",").map((x) => x.trim()).filter(Boolean);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      await stageOrg({ name: name.trim(), team: team.trim(), groups: csv(groups), members: csv(members), minAdmins: minAdmins ? Number(minAdmins) : 0 });
      setName(""); setTeam(""); setGroups(""); setMembers(""); setMinAdmins("");
      onStaged();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} style={{ marginTop: 16, display: "flex", gap: 10, flexWrap: "wrap", alignItems: "flex-end" }}>
      <label>Organization login<br /><input value={name} onChange={(e) => setName(e.target.value)} placeholder="acme-inc" required /></label>
      <label>Minimum owners<br /><input type="number" min="0" value={minAdmins} onChange={(e) => setMinAdmins(e.target.value)} style={{ width: 110 }} placeholder="0" /></label>
      <label>Seed team<br /><input value={team} onChange={(e) => setTeam(e.target.value)} placeholder="engineering" required /></label>
      <label>Groups (comma-separated)<br /><input value={groups} onChange={(e) => setGroups(e.target.value)} placeholder="eng@acme.com" /></label>
      <label>Members (comma-separated)<br /><input value={members} onChange={(e) => setMembers(e.target.value)} placeholder="octocat" /></label>
      <button type="submit" disabled={busy}>{busy ? "Staging…" : "Stage organization"}</button>
      {err && <span className="banner">{err}</span>}
    </form>
  );
}

// DirectoryForm adds an operator-added resolver-backed directory (operator-only).
function DirectoryForm({ onAdded }: { onAdded: () => void }) {
  const [name, setName] = useState("");
  const [domains, setDomains] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [probeGroup, setProbeGroup] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      await addDirectory({ name: name.trim(), domains: domains.split(",").map((x) => x.trim()).filter(Boolean), endpoint: endpoint.trim(), probeGroup: probeGroup.trim() });
      setName(""); setDomains(""); setEndpoint(""); setProbeGroup("");
      onAdded();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} style={{ marginTop: 12, display: "flex", gap: 10, flexWrap: "wrap", alignItems: "flex-end" }}>
      <label>Name<br /><input value={name} onChange={(e) => setName(e.target.value)} required /></label>
      <label>Domains (comma-separated)<br /><input value={domains} onChange={(e) => setDomains(e.target.value)} style={{ width: 240 }} required /></label>
      <label>DirectoryService endpoint<br /><input type="url" value={endpoint} onChange={(e) => setEndpoint(e.target.value)} style={{ width: 260 }} placeholder="http://google-group-sync:8090" required /></label>
      <label>Probe group (optional)<br /><input value={probeGroup} onChange={(e) => setProbeGroup(e.target.value)} /></label>
      <button type="submit" disabled={busy}>{busy ? "Adding…" : "Add directory"}</button>
      {err && <span className="banner">{err}</span>}
    </form>
  );
}

function SettingsView() {
  const [reload, setReload] = useState(0);
  const [actErr, setActErr] = useState("");
  const { data, error } = useAsync<Settings>(fetchSettings, [reload]);
  const refresh = () => setReload((r) => r + 1);
  const act = async (fn: () => Promise<void>) => {
    setActErr("");
    try { await fn(); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); }
  };
  if (error) return <div className="banner">{error}</div>;
  if (data === null) return <p className="muted">Loading…</p>;
  return (
    <>
      <p className="muted">Git-declared entries are <span className="badge muted">managed in git</span>; operator-added ones can be removed here.</p>
      <h2>Directories</h2>
      <table>
        <thead><tr><th>Name</th><th>Domains</th><th>Reads via</th><th>Probe group</th><th></th></tr></thead>
        <tbody>
          {(data.sources ?? []).map((s) => (
            <tr key={s.name}>
              <td>{s.name}</td>
              <td className="muted">{(s.domains ?? []).join(", ")}</td>
              <td>{s.endpoint ? <span className="muted">DirectoryService</span> : <span className="muted">in-process Google</span>}</td>
              <td className="muted">{s.probeGroup || "—"}</td>
              <td></td>
            </tr>
          ))}
          {(data.storeSources ?? []).map((s) => (
            <tr key={`store-${s.name}`}>
              <td>{s.name} <span className="badge muted">added here</span></td>
              <td className="muted">{(s.domains ?? []).join(", ")}</td>
              <td><span className="muted">DirectoryService</span></td>
              <td className="muted">{s.probeGroup || "—"}</td>
              <td><button className="chip" onClick={() => act(() => deleteDirectory(s.name))}>Delete</button></td>
            </tr>
          ))}
          {(data.sources ?? []).length === 0 && (data.storeSources ?? []).length === 0 && <tr><td colSpan={5} className="muted">No directories.</td></tr>}
        </tbody>
      </table>
      {actErr && <div className="banner">{actErr}</div>}
      <DirectoryForm onAdded={refresh} />
      <h2>Organizations</h2>
      {(data.orgs ?? []).map((o) => <OrgSection key={o.name} org={o} />)}
      {(data.storeOrgs ?? []).map((o) => <OrgSection key={`store-${o.name}`} org={o} staged />)}
      <h3 style={{ marginTop: 20 }}>Stage an organization</h3>
      <p className="muted">Adds an organization to the config store, born reconcile-disabled with no credentials. Once staged it shows a “Create GitHub App” link. At least one team (a group or a member) is required. Operator-only.</p>
      <StageOrgForm onStaged={() => setReload((r) => r + 1)} />
    </>
  );
}

// VersionBadge shows the running build, fetched over ConnectRPC — the first
// Connect-Web call, proving the typed client end to end.
function VersionBadge() {
  const { data } = useAsync(fetchVersion);
  if (!data) return null;
  const commit = data.commit ? ` (${data.commit.slice(0, 7)})` : "";
  return <span className="version muted" title="via ConnectRPC">{data.version}{commit}</span>;
}

// UserBadge shows the signed-in caller and role, with a sign-out link. Sign
// out goes to the oauth2-proxy gateway endpoint, which clears the session
// cookie; the next request re-runs the OIDC login.
function UserBadge() {
  const { data } = useAsync(fetchMe);
  if (!data) return null;
  const who = data.name && data.email ? `${data.name} <${data.email}>` : (data.name || data.email);
  return (
    <span className="user muted">
      {who && <>{who} </>}
      {data.role && <span className={`badge ${data.role === "operator" ? "ok" : "muted"}`}>{data.role}</span>}
      {" · "}
      <a href="/oauth2/sign_out" title="Sign out">sign out</a>
    </span>
  );
}

export function App() {
  const [tab, setTab] = useState<Tab>("people");
  const tabs: [Tab, string][] = [["people", "People"], ["status", "Status"], ["history", "History"], ["settings", "Settings"]];
  return (
    <main>
      <header>
        <span className="brand">roster</span>
        <nav>{tabs.map(([k, l]) => <a key={k} className={tab === k ? "cur" : ""} onClick={() => setTab(k)}>{l}</a>)}</nav>
        <VersionBadge />
        <UserBadge />
      </header>
      <h1>{tabs.find(([k]) => k === tab)![1]}</h1>
      {tab === "people" && <PeopleView />}
      {tab === "status" && <StatusView />}
      {tab === "history" && <HistoryView />}
      {tab === "settings" && <SettingsView />}
    </main>
  );
}
