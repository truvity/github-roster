import { useMemo, useState, type FormEvent } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import IconButton from "@mui/material/IconButton";
import MenuItem from "@mui/material/MenuItem";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TableSortLabel from "@mui/material/TableSortLabel";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import KeyboardArrowDownIcon from "@mui/icons-material/KeyboardArrowDown";
import KeyboardArrowRightIcon from "@mui/icons-material/KeyboardArrowRight";
import {
  deletePerson, fetchRoster, getPerson, putPerson,
  type Candidate, type Person, type PersonInput,
} from "./api";
import { useAsync } from "./hooks";
import { IdPDirectories, Mono, PersonTraceBody, StateChip } from "./ui";

type Preset = "needs" | "active" | "left" | "bots" | "all";
type SortKey = "state" | "name" | "github";

function personMatches(p: Person, preset: Preset, q: string): boolean {
  if (q && !`${p.name} ${p.github} ${p.email ?? ""}`.toLowerCase().includes(q.toLowerCase())) return false;
  switch (preset) {
    case "needs": return ["pending", "invited", "leaving"].includes(p.state ?? "");
    case "active": return ["synced", "pending", "invited"].includes(p.state ?? "");
    case "left": return p.state === "left";
    case "bots": return p.class === "bot";
    default: return true;
  }
}

function candidateMatches(c: Candidate, preset: Preset, q: string): boolean {
  if (q && !`${c.name} ${c.github} ${c.email ?? ""}`.toLowerCase().includes(q.toLowerCase())) return false;
  return preset === "needs" || preset === "all";
}

// PersonDialog adds or edits a roster mapping entry. With `initial` it edits:
// the name is the store key, so it is read-only and the save replaces in place.
function PersonDialog({ initial, onSaved, onClose }: { initial?: PersonInput; onSaved: () => void; onClose: () => void }) {
  const editing = initial !== undefined;
  const [name, setName] = useState(initial?.name ?? "");
  const [github, setGithub] = useState(initial?.github ?? "");
  const [k8s, setK8s] = useState(initial?.k8s ?? "");
  const [cls, setCls] = useState(initial?.class ?? "employee");
  const [emails, setEmails] = useState((initial?.emails ?? []).join(", "));
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      await putPerson({
        name: name.trim(), github: github.trim(), k8s: k8s.trim(), class: cls,
        emails: emails.split(",").map((x) => x.trim()).filter(Boolean), pinned: initial?.pinned,
      });
      onSaved();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} maxWidth="sm" fullWidth>
      <form onSubmit={submit}>
        <DialogTitle>{editing ? `Edit ${initial!.name}` : "Add person"}</DialogTitle>
        <DialogContent sx={{ display: "flex", flexDirection: "column", gap: 2, pt: "8px !important" }}>
          <TextField label="Name" value={name} onChange={(e) => setName(e.target.value)} required
            slotProps={{ input: { readOnly: editing } }}
            helperText={editing ? "The name is the entry key; Remove and re-add to rename." : undefined} />
          <TextField label="GitHub login" value={github} onChange={(e) => setGithub(e.target.value)} required />
          <TextField label="Namespace (k8s)" value={k8s} onChange={(e) => setK8s(e.target.value)} />
          <TextField label="Class" select value={cls} onChange={(e) => setCls(e.target.value)}>
            <MenuItem value="employee">employee</MenuItem>
            <MenuItem value="bot">bot</MenuItem>
          </TextField>
          <TextField label="Emails (comma-separated)" value={emails} onChange={(e) => setEmails(e.target.value)} />
          {err && <Alert severity="error">{err}</Alert>}
        </DialogContent>
        <DialogActions>
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="contained" disabled={busy}>{busy ? "Saving…" : editing ? "Save changes" : "Add person"}</Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// CandidateRows is one awaiting-approval row plus its expandable IdP trace:
// editable name + login (prefilled from the side the join knows), one-click
// Approve (NEW) / Adopt (UNKNOWN).
function CandidateRows({ c, onApproved }: { c: Candidate; onApproved: () => void }) {
  const [name, setName] = useState(c.name);
  const [github, setGithub] = useState(c.github);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [open, setOpen] = useState(false);
  const hasTrace = Object.keys(c.directories ?? {}).length > 0;
  const approve = async () => {
    setErr(""); setBusy(true);
    try {
      await putPerson({ name: name.trim(), github: github.trim(), k8s: name.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-"), class: "employee" });
      onApproved();
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)); setBusy(false); }
  };
  const ready = name.trim() !== "" && github.trim() !== "";
  return (
    <>
      <TableRow hover>
        <TableCell><StateChip state={c.kind === "unknown" ? "unknown" : "new"} /></TableCell>
        <TableCell>
          <Box sx={{ display: "flex", alignItems: "center", gap: 0.5 }}>
            {hasTrace && (
              <IconButton size="small" onClick={() => setOpen((o) => !o)} title="Show the IdP identity">
                {open ? <KeyboardArrowDownIcon fontSize="small" /> : <KeyboardArrowRightIcon fontSize="small" />}
              </IconButton>
            )}
            <TextField value={name} onChange={(e) => setName(e.target.value)} placeholder="Full Name" sx={{ width: 190 }} />
          </Box>
          {c.email && <Typography variant="caption" color="text.secondary" sx={{ ml: hasTrace ? 4.5 : 0 }}>{c.email}</Typography>}
        </TableCell>
        <TableCell><TextField value={github} onChange={(e) => setGithub(e.target.value)} placeholder="login" sx={{ width: 170 }} slotProps={{ input: { sx: { fontFamily: "monospace" } } }} /></TableCell>
        <TableCell>
          <Typography variant="body2" color="text.secondary">{c.org ? `${c.org}: ` : ""}{c.detail}</Typography>
        </TableCell>
        <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
          <Button variant="contained" disabled={!ready || busy} onClick={approve}
            title={c.kind === "unknown" ? "Adopt this member" : "Approve & invite"}>
            {busy ? "…" : c.kind === "unknown" ? "Adopt" : "Approve"}
          </Button>
          {err && <Alert severity="error" sx={{ mt: 1 }}>{err}</Alert>}
        </TableCell>
      </TableRow>
      {hasTrace && (
        <TableRow>
          <TableCell colSpan={5} sx={{ py: 0, border: open ? undefined : 0 }}>
            <Collapse in={open} unmountOnExit>
              <Box sx={{ py: 1 }}><IdPDirectories directories={c.directories} /></Box>
            </Collapse>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

// PersonRows is one mapped person plus their expandable identity trace.
function PersonRows({ p, onEdit, onRemove }: { p: Person; onEdit: () => void; onRemove: () => void }) {
  const [open, setOpen] = useState(false);
  const orgs = Object.entries(p.orgs ?? {}).filter(([, m]) => m.state);
  return (
    <>
      <TableRow hover>
        <TableCell><StateChip state={p.state} /></TableCell>
        <TableCell>
          <Box sx={{ display: "flex", alignItems: "center", gap: 0.5 }}>
            <IconButton size="small" onClick={() => setOpen((o) => !o)} title="Show the identity trace (IdP → mapping → orgs)">
              {open ? <KeyboardArrowDownIcon fontSize="small" /> : <KeyboardArrowRightIcon fontSize="small" />}
            </IconButton>
            <Box>
              <Typography variant="body2">{p.name}</Typography>
              {p.email && <Typography variant="caption" color="text.secondary">{p.email}</Typography>}
            </Box>
          </Box>
        </TableCell>
        <TableCell><Mono>{p.github || "—"}</Mono></TableCell>
        <TableCell>
          <Box sx={{ display: "flex", gap: 0.5, flexWrap: "wrap" }}>
            {orgs.length === 0 && <Typography variant="body2" color="text.secondary">—</Typography>}
            {orgs.map(([org, m]) => (
              <Chip key={org} label={`${org}: ${m.state}`} color={m.state === "synced" ? "success" : m.state === "leaving" ? "error" : "warning"} variant="outlined" />
            ))}
          </Box>
        </TableCell>
        <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
          <Button onClick={onEdit} title="Edit this entry">Edit</Button>
          <Button color="error" onClick={onRemove} title="Remove from the roster">Remove</Button>
        </TableCell>
      </TableRow>
      <TableRow>
        <TableCell colSpan={5} sx={{ py: 0, border: open ? undefined : 0 }}>
          <Collapse in={open} unmountOnExit><PersonTraceBody p={p} /></Collapse>
        </TableCell>
      </TableRow>
    </>
  );
}

export function PeopleView() {
  const [reload, setReload] = useState(0);
  const [actErr, setActErr] = useState("");
  const [preset, setPreset] = useState<Preset>("needs");
  const [q, setQ] = useState("");
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<PersonInput | null>(null);
  const [sortBy, setSortBy] = useState<SortKey>("name");
  const [asc, setAsc] = useState(true);
  const { data, error } = useAsync((s) => fetchRoster(s), [reload]);
  const refresh = () => setReload((r) => r + 1);

  const remove = async (name: string) => {
    setActErr("");
    try { await deletePerson(name); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); }
  };
  const edit = async (name: string) => {
    setActErr("");
    try { setEditing(await getPerson(name)); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); }
  };
  const sortToggle = (k: SortKey) => {
    if (sortBy === k) setAsc((a) => !a);
    else { setSortBy(k); setAsc(true); }
  };

  const people = data?.people ?? null;
  const candidates = data?.candidates ?? [];
  const shownCandidates = useMemo(() => {
    const list = candidates.filter((c) => candidateMatches(c, preset, q));
    const dir = asc ? 1 : -1;
    return [...list].sort((a, b) => dir * `${a.name}${a.github}`.localeCompare(`${b.name}${b.github}`));
  }, [candidates, preset, q, asc]);
  const rows = useMemo(() => {
    const dir = asc ? 1 : -1;
    const key = (p: Person) => (sortBy === "github" ? p.github : sortBy === "state" ? (p.state ?? "") : p.name);
    return (people ?? []).filter((p) => personMatches(p, preset, q)).sort((a, b) => dir * key(a).localeCompare(key(b)));
  }, [people, preset, q, sortBy, asc]);

  if (error) return <Alert severity="error">{error}</Alert>;
  if (people === null) return <Typography color="text.secondary">Loading…</Typography>;

  const needs = candidates.length;
  const presets: [Preset, string][] = [
    ["needs", needs ? `Needs me (${needs})` : "Needs me"], ["active", "Active"], ["left", "Left"], ["bots", "Bots"], ["all", "All"],
  ];
  const empty = shownCandidates.length === 0 && rows.length === 0;

  return (
    <>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap", mb: 2 }}>
        {presets.map(([k, l]) => (
          <Chip key={k} label={l} clickable color={preset === k ? "primary" : "default"} onClick={() => setPreset(k)} />
        ))}
        <TextField placeholder="name, login or email…" value={q} onChange={(e) => setQ(e.target.value)} sx={{ width: 220 }} />
        <Button variant="contained" sx={{ ml: "auto" }} onClick={() => setAdding(true)}>+ Add person</Button>
      </Box>
      {actErr && <Alert severity="error" sx={{ mb: 2 }}>{actErr}</Alert>}
      {adding && <PersonDialog onSaved={() => { setAdding(false); refresh(); }} onClose={() => setAdding(false)} />}
      {editing && <PersonDialog initial={editing} onSaved={() => { setEditing(null); refresh(); }} onClose={() => setEditing(null)} />}
      <TableContainer component={Paper} variant="outlined" sx={{ maxHeight: "calc(100vh - 220px)" }}>
        <Table stickyHeader size="small">
          <TableHead>
            <TableRow>
              <TableCell sx={{ width: 90 }}>
                <TableSortLabel active={sortBy === "state"} direction={asc ? "asc" : "desc"} onClick={() => sortToggle("state")}>State</TableSortLabel>
              </TableCell>
              <TableCell>
                <TableSortLabel active={sortBy === "name"} direction={asc ? "asc" : "desc"} onClick={() => sortToggle("name")}>Name</TableSortLabel>
              </TableCell>
              <TableCell>
                <TableSortLabel active={sortBy === "github"} direction={asc ? "asc" : "desc"} onClick={() => sortToggle("github")}>GitHub</TableSortLabel>
              </TableCell>
              <TableCell>Organizations</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {shownCandidates.map((c, i) => <CandidateRows key={`c-${c.kind}-${c.name}-${c.github}-${i}`} c={c} onApproved={refresh} />)}
            {rows.map((p) => <PersonRows key={p.name} p={p} onEdit={() => edit(p.name)} onRemove={() => remove(p.name)} />)}
            {empty && <TableRow><TableCell colSpan={5}><Typography color="text.secondary">Nobody matches this filter.</Typography></TableCell></TableRow>}
          </TableBody>
        </Table>
      </TableContainer>
    </>
  );
}
