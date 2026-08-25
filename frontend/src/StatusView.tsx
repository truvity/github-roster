import { useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Typography from "@mui/material/Typography";
import KeyboardArrowDownIcon from "@mui/icons-material/KeyboardArrowDown";
import KeyboardArrowRightIcon from "@mui/icons-material/KeyboardArrowRight";
import { fetchStatus, runReconcile, setPaused, setReconcileEnabled, type ReconcileStatus } from "./api";
import { useAsync } from "./hooks";
import { Mono } from "./ui";

function verbColor(verb: string): "success" | "error" | "warning" | "default" {
  if (verb.startsWith("invite")) return "success";
  if (verb === "remove" || verb === "cancel-invite") return "error";
  if (verb.startsWith("role")) return "warning";
  return "default";
}

function Pending({ s, open, toggle }: { s: ReconcileStatus; open: boolean; toggle: () => void }) {
  if (!s.actions) return <Typography variant="body2" color="text.secondary">in sync</Typography>;
  const hasDetails = (s.details?.length ?? 0) > 0;
  const badges = (
    <Box sx={{ display: "inline-flex", gap: 0.5, flexWrap: "wrap", alignItems: "center" }}>
      {s.adds ? <Chip label={`+${s.adds} invite`} color="success" variant="outlined" /> : null}
      {s.removes ? <Chip label={`−${s.removes} remove`} color="error" variant="outlined" /> : null}
      {s.roleChanges ? <Chip label={`${s.roleChanges} role`} color="warning" variant="outlined" /> : null}
      {s.teamChanges ? <Chip label={`${s.teamChanges} team`} variant="outlined" /> : null}
      {hasDetails ? (open ? <KeyboardArrowDownIcon fontSize="small" color="disabled" /> : <KeyboardArrowRightIcon fontSize="small" color="disabled" />) : null}
    </Box>
  );
  if (!hasDetails) return badges;
  return (
    <Box component="button" type="button" onClick={toggle} title="Show the exact changes"
      sx={{ background: "none", border: "none", p: 0, cursor: "pointer", font: "inherit" }}>
      {badges}
    </Box>
  );
}

function Outcome({ s }: { s: ReconcileStatus }) {
  if (s.error) return <Chip label="failed" color="error" title={s.error} />;
  if (s.held) return <><Chip label="held" color="warning" /> <Typography component="span" variant="body2" color="text.secondary">{s.reason}</Typography></>;
  if (s.applied) return <Chip label="applied" color="success" />;
  if (!s.actions) return <Chip label="in sync" color="success" variant="outlined" />;
  return s.enabled
    ? <Typography variant="body2" color="text.secondary">applying next tick</Typography>
    : <Chip label="would apply on enable" color="warning" variant="outlined" />;
}

export function StatusView() {
  const [reload, setReload] = useState(0);
  const { data, error } = useAsync(fetchStatus, [reload]);
  const [busy, setBusy] = useState("");
  const [actErr, setActErr] = useState("");
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const refresh = () => setReload((r) => r + 1);
  const act = async (key: string, fn: () => Promise<void>) => {
    setActErr(""); setBusy(key);
    try { await fn(); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(""); }
  };
  if (error) return <Alert severity="error">{error}</Alert>;
  if (data === null) return <Typography color="text.secondary">Loading…</Typography>;
  const toggleEnabled = (s: ReconcileStatus) => {
    if (!s.enabled && !window.confirm(`Enable the reconcile loop for ${s.org}? It will start applying the pending changes to GitHub.`)) return;
    act(`enable-${s.org}`, () => setReconcileEnabled(s.org, !s.enabled));
  };
  return (
    <>
      <Box sx={{ mb: 2 }}>
        <Button variant="contained" disabled={busy !== ""} onClick={() => act("run", runReconcile)}>
          {busy === "run" ? "Reconciling…" : "Sync now"}
        </Button>
      </Box>
      {actErr && <Alert severity="error" sx={{ mb: 2 }}>{actErr}</Alert>}
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Organization</TableCell><TableCell>Loop</TableCell><TableCell>Pending</TableCell>
              <TableCell>Last run</TableCell><TableCell>Outcome</TableCell><TableCell>Controls</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {data.map((s) => (
              <>
                <TableRow key={s.org} hover>
                  <TableCell>{s.org}</TableCell>
                  <TableCell>
                    <Chip label={s.enabled ? "enabled" : "disabled"} color={s.enabled ? "success" : "warning"} />
                    {s.paused && <Chip label="paused" color="error" sx={{ ml: 0.5 }} />}
                  </TableCell>
                  <TableCell><Pending s={s} open={!!open[s.org]} toggle={() => setOpen((o) => ({ ...o, [s.org]: !o[s.org] }))} /></TableCell>
                  <TableCell><Typography variant="body2" color="text.secondary">{s.at ? s.at.slice(0, 16).replace("T", " ") : "—"}</Typography></TableCell>
                  <TableCell><Outcome s={s} /></TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    <Button disabled={busy !== ""} onClick={() => toggleEnabled(s)}
                      title={s.enabled ? "Stop the reconcile loop" : "Start the reconcile loop (applies changes)"}>
                      {s.enabled ? "Disable" : "Enable"}
                    </Button>
                    {s.enabled && (
                      <Button disabled={busy !== ""} onClick={() => act(`pause-${s.org}`, () => setPaused(s.org, !s.paused))}
                        title="Temporarily halt without disabling">
                        {s.paused ? "Resume" : "Pause"}
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
                <TableRow key={`${s.org}-d`}>
                  <TableCell colSpan={6} sx={{ py: 0, border: open[s.org] ? undefined : 0 }}>
                    <Collapse in={!!open[s.org]} unmountOnExit>
                      <Box sx={{ display: "flex", flexDirection: "column", gap: 0.5, py: 1 }}>
                        {(s.details ?? []).map((d, i) => (
                          <Box key={i} sx={{ display: "flex", alignItems: "center", gap: 1 }}>
                            <Chip label={d.verb} color={verbColor(d.verb)} variant="outlined" />
                            <Mono>{d.login || "—"}</Mono>
                            {d.team && <Typography variant="body2" color="text.secondary">· team {d.team}</Typography>}
                          </Box>
                        ))}
                      </Box>
                    </Collapse>
                  </TableCell>
                </TableRow>
              </>
            ))}
            {data.length === 0 && <TableRow><TableCell colSpan={6}><Typography color="text.secondary">No reconcile status yet.</Typography></TableCell></TableRow>}
          </TableBody>
        </Table>
      </TableContainer>
    </>
  );
}
