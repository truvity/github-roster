import { useEffect, useMemo, useState } from "react";
import { fetchRoster, type Person } from "./api";

type Preset = "needs" | "active" | "left" | "bots" | "all";

function personBadge(state?: string): { label: string; cls: string } {
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

function matches(p: Person, preset: Preset, q: string): boolean {
  if (q) {
    const hay = `${p.name} ${p.github}`.toLowerCase();
    if (!hay.includes(q.toLowerCase())) return false;
  }
  switch (preset) {
    case "needs": return p.state === "pending" || p.state === "invited" || p.state === "leaving";
    case "active": return p.state === "synced" || p.state === "pending" || p.state === "invited";
    case "left": return p.state === "left";
    case "bots": return p.class === "bot";
    default: return true;
  }
}

export function App() {
  const [people, setPeople] = useState<Person[] | null>(null);
  const [error, setError] = useState<string>("");
  const [preset, setPreset] = useState<Preset>("all");
  const [q, setQ] = useState("");

  useEffect(() => {
    const ac = new AbortController();
    fetchRoster(ac.signal)
      .then((r) => setPeople(r.people ?? []))
      .catch((e) => { if (e.name !== "AbortError") setError(String(e)); });
    return () => ac.abort();
  }, []);

  const orgs = useMemo(() => {
    const set = new Set<string>();
    (people ?? []).forEach((p) => Object.keys(p.orgs ?? {}).forEach((o) => set.add(o)));
    return [...set].sort();
  }, [people]);

  const rows = useMemo(
    () => (people ?? []).filter((p) => matches(p, preset, q)).sort((a, b) => a.name.localeCompare(b.name)),
    [people, preset, q],
  );

  const presets: [Preset, string][] = [
    ["needs", "Needs me"], ["active", "Active"], ["left", "Left"], ["bots", "Bots"], ["all", "All"],
  ];

  return (
    <main>
      <header>
        <span className="brand">roster</span>
        <nav><a className="cur">People</a></nav>
      </header>

      <h1>People</h1>

      {error && <div className="banner">{error}</div>}

      <div className="presets">
        {presets.map(([key, label]) => (
          <button key={key} className={preset === key ? "chip cur" : "chip"} onClick={() => setPreset(key)}>
            {label}
          </button>
        ))}
        <input placeholder="name or login…" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>

      {people === null ? (
        <p className="muted">Loading…</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th><th>GitHub</th><th>State</th>
              {orgs.map((o) => <th key={o}>{o}</th>)}
            </tr>
          </thead>
          <tbody>
            {rows.map((p) => {
              const b = personBadge(p.state);
              return (
                <tr key={p.name}>
                  <td>{p.name}</td>
                  <td className="mono">{p.github || "—"}</td>
                  <td><span className={`badge ${b.cls}`}>{b.label}</span></td>
                  {orgs.map((o) => {
                    const m = p.orgs?.[o];
                    const s = m?.state;
                    return <td key={o}>{s ? <span className={`badge ${personBadge(s).cls}`}>{personBadge(s).label}</span> : <span className="muted">—</span>}</td>;
                  })}
                </tr>
              );
            })}
            {rows.length === 0 && <tr><td colSpan={3 + orgs.length} className="muted">Nobody matches.</td></tr>}
          </tbody>
        </table>
      )}
    </main>
  );
}
