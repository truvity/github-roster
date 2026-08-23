import { useEffect, useMemo, useState } from "react";
import {
  fetchRoster, fetchStatus, fetchAudit, flattenAudit,
  type Person, type ReconcileStatus, type Change,
} from "./api";

type Tab = "people" | "status" | "history";

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

// useAsync runs loader once and tracks {data,error}.
function useAsync<T>(loader: (s: AbortSignal) => Promise<T>): { data: T | null; error: string } {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const ac = new AbortController();
    loader(ac.signal).then(setData).catch((e) => { if (e.name !== "AbortError") setError(String(e)); });
    return () => ac.abort();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
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

function PeopleView() {
  const { data: people, error } = useAsync((s) => fetchRoster(s).then((r) => r.people ?? []));
  const [preset, setPreset] = useState<Preset>("all");
  const [q, setQ] = useState("");
  const orgs = useMemo(() => {
    const set = new Set<string>();
    (people ?? []).forEach((p) => Object.keys(p.orgs ?? {}).forEach((o) => set.add(o)));
    return [...set].sort();
  }, [people]);
  const rows = useMemo(
    () => (people ?? []).filter((p) => peopleMatches(p, preset, q)).sort((a, b) => a.name.localeCompare(b.name)),
    [people, preset, q],
  );
  if (error) return <div className="banner">{error}</div>;
  if (people === null) return <p className="muted">Loading…</p>;
  const presets: [Preset, string][] = [["needs", "Needs me"], ["active", "Active"], ["left", "Left"], ["bots", "Bots"], ["all", "All"]];
  return (
    <>
      <div className="presets">
        {presets.map(([k, l]) => <button key={k} className={preset === k ? "chip cur" : "chip"} onClick={() => setPreset(k)}>{l}</button>)}
        <input placeholder="name or login…" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>
      <table>
        <thead><tr><th>Name</th><th>GitHub</th><th>State</th>{orgs.map((o) => <th key={o}>{o}</th>)}</tr></thead>
        <tbody>
          {rows.map((p) => {
            const b = badge(p.state);
            return (
              <tr key={p.name}>
                <td>{p.name}</td><td className="mono">{p.github || "—"}</td>
                <td><span className={`badge ${b.cls}`}>{b.label}</span></td>
                {orgs.map((o) => {
                  const s = p.orgs?.[o]?.state;
                  const ob = badge(s);
                  return <td key={o}>{s ? <span className={`badge ${ob.cls}`}>{ob.label}</span> : <span className="muted">—</span>}</td>;
                })}
              </tr>
            );
          })}
          {rows.length === 0 && <tr><td colSpan={3 + orgs.length} className="muted">Nobody matches.</td></tr>}
        </tbody>
      </table>
    </>
  );
}

function StatusView() {
  const { data, error } = useAsync(fetchStatus);
  if (error) return <div className="banner">{error}</div>;
  if (data === null) return <p className="muted">Loading…</p>;
  const outcome = (s: ReconcileStatus) => {
    if (s.error) return <span className="badge danger">failed</span>;
    if (s.held) return <><span className="badge warn">held</span> <span className="muted">{s.reason}</span></>;
    if (s.applied) return <span className="badge ok">applied</span>;
    if (!s.enabled) return s.actions ? <span className="badge warn">would change {s.actions}</span> : <span className="badge ok">in sync</span>;
    return s.actions ? <span className="muted">pending next tick</span> : <span className="badge ok">in sync</span>;
  };
  return (
    <table>
      <thead><tr><th>Organization</th><th>Loop</th><th>Pending</th><th>Last run</th><th>Outcome</th></tr></thead>
      <tbody>
        {data.map((s) => (
          <tr key={s.org}>
            <td>{s.org}</td>
            <td>{s.enabled ? <span className="badge ok">enabled</span> : <span className="badge warn">disabled</span>}{s.paused && <> <span className="badge danger">paused</span></>}</td>
            <td>{s.actions || <span className="muted">none</span>}</td>
            <td className="muted">{s.at ? s.at.slice(0, 16).replace("T", " ") : "—"}</td>
            <td>{outcome(s)}</td>
          </tr>
        ))}
        {data.length === 0 && <tr><td colSpan={5} className="muted">No reconcile status yet.</td></tr>}
      </tbody>
    </table>
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

export function App() {
  const [tab, setTab] = useState<Tab>("people");
  const tabs: [Tab, string][] = [["people", "People"], ["status", "Status"], ["history", "History"]];
  return (
    <main>
      <header>
        <span className="brand">roster</span>
        <nav>{tabs.map(([k, l]) => <a key={k} className={tab === k ? "cur" : ""} onClick={() => setTab(k)}>{l}</a>)}</nav>
      </header>
      <h1>{tabs.find(([k]) => k === tab)![1]}</h1>
      {tab === "people" && <PeopleView />}
      {tab === "status" && <StatusView />}
      {tab === "history" && <HistoryView />}
    </main>
  );
}
