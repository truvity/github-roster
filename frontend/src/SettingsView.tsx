import { useState, type FormEvent } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { addDirectory, deleteDirectory, deleteOrgTeam, fetchSettings, putOrgTeam, stageOrg, type Settings, type SettingsDomain, type SettingsOrg } from "./api";
import { useAsync } from "./hooks";

// DomainList renders a directory's domains, each with its probe group and its
// sync switch — one row per domain, so a multi-domain Workspace reads as the
// independent domains it actually is.
function DomainList({ domains }: { domains?: SettingsDomain[] }) {
  if (!domains?.length) return <Typography variant="body2" color="text.secondary">—</Typography>;
  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: 0.5 }}>
      {domains.map((d) => (
        <Box key={d.domain} sx={{ display: "flex", alignItems: "center", gap: 1 }}>
          <Typography variant="body2">{d.domain}</Typography>
          {d.probeGroup
            ? <Chip label={`probe: ${d.probeGroup}`} variant="outlined" />
            : <Chip label="no probe" variant="outlined" color="default" />}
          <Chip label={d.sync === false ? "display only" : "synced"} color={d.sync === false ? "warning" : "success"} variant="outlined" />
        </Box>
      ))}
    </Box>
  );
}

// OrgSection renders one organization and its teams; `staged` marks a
// store-added org (present in the config store, born disabled, not yet run).
// TeamEditor adds or replaces one team mapping on a staged org.
function TeamEditor({ org, initial, onSaved }: { org: string; initial?: { name: string; groups?: string[]; members?: string[] }; onSaved: () => void }) {
  const [team, setTeam] = useState(initial?.name ?? "");
  const [groups, setGroups] = useState((initial?.groups ?? []).join(", "));
  const [members, setMembers] = useState((initial?.members ?? []).join(", "));
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const csv = (v: string) => v.split(",").map((x) => x.trim()).filter(Boolean);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      await putOrgTeam({ org, team: team.trim(), groups: csv(groups), members: csv(members) });
      setTeam(""); setGroups(""); setMembers("");
      onSaved();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Box component="form" onSubmit={submit} sx={{ mt: 1, display: "flex", gap: 1, flexWrap: "wrap", alignItems: "center" }}>
      <TextField label="Team" value={team} onChange={(e) => setTeam(e.target.value)} required sx={{ width: 170 }} />
      <TextField label="Groups (comma-separated)" value={groups} onChange={(e) => setGroups(e.target.value)} sx={{ width: 240 }} />
      <TextField label="Members (comma-separated)" value={members} onChange={(e) => setMembers(e.target.value)} sx={{ width: 220 }} />
      <Button type="submit" variant="outlined" disabled={busy}>{busy ? "Saving…" : initial ? "Save team" : "Add team"}</Button>
      {err && <Alert severity="error">{err}</Alert>}
    </Box>
  );
}

function OrgSection({ org: o, staged, onChanged }: { org: SettingsOrg; staged?: boolean; onChanged?: () => void }) {
  const [actErr, setActErr] = useState("");
  const [editing, setEditing] = useState<string | null>(null);
  const removeTeam = async (team: string) => {
    setActErr("");
    try { await deleteOrgTeam(o.name, team); onChanged?.(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); }
  };
  return (
    <Box component="section" sx={{ mt: 3 }}>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1, mb: 1 }}>
        <Typography variant="h3">{o.name}</Typography>
        {staged
          ? <Chip label={`${o.provenance === "roster" ? "created by roster" : "added manually"} (staged)`} color="warning" />
          : <Chip label={`home: ${o.company}`} variant="outlined" />}
        <Chip label={o.reconcileEnabled ? "loop enabled" : "loop disabled"} color={o.reconcileEnabled ? "success" : "warning"} variant="outlined" />
      </Box>
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead><TableRow><TableCell>Team</TableCell><TableCell>Membership from</TableCell>{staged && <TableCell />}</TableRow></TableHead>
          <TableBody>
            {(o.teams ?? []).map((t) => (
              <TableRow key={t.name} hover>
                <TableCell>{t.name}</TableCell>
                <TableCell>
                  <Typography variant="body2" color="text.secondary">
                    {t.pinned ? "pinned (operator-edited)" : [...(t.groups ?? []), ...(t.members ?? []).map((m) => `+${m}`)].join(", ")}
                  </Typography>
                </TableCell>
                {staged && (
                  <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                    <Button onClick={() => setEditing(editing === t.name ? null : t.name)}>{editing === t.name ? "Close" : "Edit"}</Button>
                    <Button color="error" onClick={() => removeTeam(t.name)}>Delete</Button>
                  </TableCell>
                )}
              </TableRow>
            ))}
            {(o.teams ?? []).length === 0 && <TableRow><TableCell colSpan={staged ? 3 : 2}><Typography color="text.secondary">No teams.</Typography></TableCell></TableRow>}
          </TableBody>
        </Table>
      </TableContainer>
      {actErr && <Alert severity="error" sx={{ mt: 1 }}>{actErr}</Alert>}
      {staged && (
        <TeamEditor org={o.name}
          initial={editing ? { name: editing, groups: (o.teams ?? []).find((t) => t.name === editing)?.groups, members: (o.teams ?? []).find((t) => t.name === editing)?.members } : undefined}
          key={editing ?? "new"}
          onSaved={() => { setEditing(null); onChanged?.(); }} />
      )}
      {staged && (
        <Typography variant="body2" sx={{ mt: 1 }}>
          <Link href={`/settings/orgs/create-app?org=${encodeURIComponent(o.name)}`}>Create GitHub App →</Link>{" "}
          <Typography component="span" variant="body2" color="text.secondary">
            redirects to GitHub; the Org Owner creates and installs it, then roster stores the credentials
          </Typography>
        </Typography>
      )}
    </Box>
  );
}

// StageOrgForm stages a new organization in the config store (operator-only).
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
    <Box component="form" onSubmit={submit} sx={{ mt: 2, display: "flex", gap: 1.5, flexWrap: "wrap", alignItems: "center" }}>
      <TextField label="Organization login" value={name} onChange={(e) => setName(e.target.value)} placeholder="acme-inc" required />
      <TextField label="Minimum owners" type="number" value={minAdmins} onChange={(e) => setMinAdmins(e.target.value)} sx={{ width: 130 }} placeholder="0" />
      <TextField label="Seed team" value={team} onChange={(e) => setTeam(e.target.value)} placeholder="engineering" required />
      <TextField label="Groups (comma-separated)" value={groups} onChange={(e) => setGroups(e.target.value)} placeholder="eng@acme.com" />
      <TextField label="Members (comma-separated)" value={members} onChange={(e) => setMembers(e.target.value)} placeholder="octocat" />
      <Button type="submit" variant="contained" disabled={busy}>{busy ? "Staging…" : "Stage organization"}</Button>
      {err && <Alert severity="error">{err}</Alert>}
    </Box>
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
    <Box component="form" onSubmit={submit} sx={{ mt: 2, display: "flex", gap: 1.5, flexWrap: "wrap", alignItems: "center" }}>
      <TextField label="Name" value={name} onChange={(e) => setName(e.target.value)} required />
      <TextField label="Domains (comma-separated)" value={domains} onChange={(e) => setDomains(e.target.value)} sx={{ width: 250 }} required />
      <TextField label="DirectoryService endpoint" type="url" value={endpoint} onChange={(e) => setEndpoint(e.target.value)} sx={{ width: 270 }} placeholder="http://google-group-sync:8090" required />
      <TextField label="Probe group (optional)" value={probeGroup} onChange={(e) => setProbeGroup(e.target.value)} />
      <Button type="submit" variant="contained" disabled={busy}>{busy ? "Adding…" : "Add directory"}</Button>
      {err && <Alert severity="error">{err}</Alert>}
    </Box>
  );
}

export function SettingsView() {
  const [reload, setReload] = useState(0);
  const [actErr, setActErr] = useState("");
  const { data, error } = useAsync<Settings>(fetchSettings, [reload]);
  const refresh = () => setReload((r) => r + 1);
  const act = async (fn: () => Promise<void>) => {
    setActErr("");
    try { await fn(); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); }
  };
  if (error) return <Alert severity="error">{error}</Alert>;
  if (data === null) return <Typography color="text.secondary">Loading…</Typography>;
  return (
    <>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Git-declared entries are <Chip label="managed in git" variant="outlined" /> ; operator-added ones can be removed here.
      </Typography>
      <Typography variant="h2" sx={{ mb: 1 }}>Directories</Typography>
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Name</TableCell><TableCell>Domains</TableCell><TableCell>Reads via</TableCell><TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {(data.sources ?? []).map((s) => (
              <TableRow key={s.name} hover>
                <TableCell>{s.name}</TableCell>
                <TableCell><DomainList domains={s.domains} /></TableCell>
                <TableCell><Typography variant="body2" color="text.secondary">{s.endpoint ? "DirectoryService" : "in-process Google"}</Typography></TableCell>
                <TableCell />
              </TableRow>
            ))}
            {(data.storeSources ?? []).map((s) => (
              <TableRow key={`store-${s.name}`} hover>
                <TableCell>{s.name} <Chip label="added here" variant="outlined" /></TableCell>
                <TableCell><DomainList domains={s.domains} /></TableCell>
                <TableCell><Typography variant="body2" color="text.secondary">DirectoryService</Typography></TableCell>
                <TableCell><Button color="error" onClick={() => act(() => deleteDirectory(s.name))}>Delete</Button></TableCell>
              </TableRow>
            ))}
            {(data.sources ?? []).length === 0 && (data.storeSources ?? []).length === 0 && (
              <TableRow><TableCell colSpan={4}><Typography color="text.secondary">No directories.</Typography></TableCell></TableRow>
            )}
          </TableBody>
        </Table>
      </TableContainer>
      {actErr && <Alert severity="error" sx={{ mt: 2 }}>{actErr}</Alert>}
      <DirectoryForm onAdded={refresh} />
      <Typography variant="h2" sx={{ mt: 4 }}>Organizations</Typography>
      {(data.orgs ?? []).map((o) => <OrgSection key={o.name} org={o} />)}
      {(data.storeOrgs ?? []).map((o) => <OrgSection key={`store-${o.name}`} org={o} staged onChanged={refresh} />)}
      <Typography variant="h3" sx={{ mt: 4 }}>Stage an organization</Typography>
      <Typography variant="body2" color="text.secondary">
        Adds an organization to the config store, born reconcile-disabled with no credentials. Once staged it shows a
        “Create GitHub App” link. At least one team (a group or a member) is required. Operator-only.
      </Typography>
      <StageOrgForm onStaged={refresh} />
    </>
  );
}
