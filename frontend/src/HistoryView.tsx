import { useMemo, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { fetchAudit, flattenAudit, type Change } from "./api";
import { useAsync } from "./hooks";
import { Mono } from "./ui";

function verbColor(v: string): "success" | "error" | "default" {
  switch (v) {
    case "added": case "team-added": return "success";
    case "removed": case "team-removed": case "failed": return "error";
    default: return "default";
  }
}

export function HistoryView() {
  const { data, error } = useAsync((s) => fetchAudit(s).then(flattenAudit));
  const [q, setQ] = useState("");
  const rows = useMemo(
    () => (data ?? []).filter((c) => !q || `${c.subject} ${c.actor}`.toLowerCase().includes(q.toLowerCase())),
    [data, q],
  );
  if (error) return <Alert severity="error">{error}</Alert>;
  if (data === null) return <Typography color="text.secondary">Loading…</Typography>;
  return (
    <>
      <Box sx={{ mb: 2 }}>
        <TextField placeholder="login or actor…" value={q} onChange={(e) => setQ(e.target.value)} sx={{ width: 220 }} />
      </Box>
      <TableContainer component={Paper} variant="outlined" sx={{ maxHeight: "calc(100vh - 220px)" }}>
        <Table stickyHeader size="small">
          <TableHead>
            <TableRow>
              <TableCell>When</TableCell><TableCell>Org</TableCell><TableCell>Kind</TableCell>
              <TableCell>Change</TableCell><TableCell>Who</TableCell><TableCell>By</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((c: Change, i) => (
              <TableRow key={i} hover>
                <TableCell><Typography variant="body2" color="text.secondary">{c.at}</Typography></TableCell>
                <TableCell>{c.org}</TableCell>
                <TableCell><Chip label={c.kind ?? "—"} variant="outlined" /></TableCell>
                <TableCell>
                  <Chip label={c.verb} color={verbColor(c.verb)} variant="outlined" />
                  {c.detail && <Typography component="span" variant="body2" color="text.secondary" sx={{ ml: 1 }}>{c.detail}</Typography>}
                </TableCell>
                <TableCell><Mono>{c.subject || "—"}</Mono></TableCell>
                <TableCell><Typography variant="body2" color="text.secondary">{c.actor}</Typography></TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && <TableRow><TableCell colSpan={6}><Typography color="text.secondary">No changes match.</Typography></TableCell></TableRow>}
          </TableBody>
        </Table>
      </TableContainer>
    </>
  );
}
