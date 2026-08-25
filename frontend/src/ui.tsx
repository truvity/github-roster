import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Typography from "@mui/material/Typography";
import { stateColor } from "./theme";
import type { DirectoryIdentity, Membership, Person } from "./api";

// Mono renders inline monospace (logins, emails, team slugs).
export function Mono({ children }: { children: React.ReactNode }) {
  return <Box component="span" sx={{ fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", fontSize: "0.92em" }}>{children}</Box>;
}

// StateChip is the lifecycle badge (synced/pending/invited/leaving/left/…).
export function StateChip({ state }: { state?: string }) {
  if (!state) return <Chip label="—" variant="outlined" />;
  return <Chip label={state} color={stateColor(state)} variant={state === "left" ? "outlined" : "filled"} />;
}

// IdPDirectories renders the per-source identity list: which directory knows a
// person, under which address, live or suspended. Shared by member and
// candidate traces — the operator's "how did we find this person".
export function IdPDirectories({ directories, missing }: { directories?: Record<string, DirectoryIdentity>; missing?: string[] }) {
  const dirs = Object.entries(directories ?? {});
  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
      <Typography variant="body2" color="text.secondary">IdP directories:</Typography>
      {dirs.length === 0 && <Typography variant="body2" color="text.secondary">none — in no directory</Typography>}
      {dirs.map(([src, d]) => (
        <Box key={src} sx={{ display: "inline-flex", alignItems: "center", gap: 0.5 }}>
          <Typography variant="body2" sx={{ fontWeight: 600 }}>{src}</Typography>
          <Mono>{d.email || "?"}</Mono>
          <Chip label={d.live ? "live" : "suspended"} color={d.live ? "success" : "default"} variant="outlined" />
        </Box>
      ))}
      {(missing ?? []).length > 0 && <Chip label={`missing from: ${missing!.join(", ")}`} color="error" variant="outlined" />}
    </Box>
  );
}

// OrgTrace renders one org's membership detail inside a person's expanded row:
// state, role, managed teams, and the current→desired diff that explains a
// pending state.
export function OrgTrace({ org, m }: { org: string; m: Membership }) {
  const cur = m.teams ?? [], des = m.desiredTeams ?? [];
  const add = des.filter((t) => !cur.includes(t)), del = cur.filter((t) => !des.includes(t));
  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}>
      <Typography variant="body2" sx={{ fontWeight: 600 }}>{org}</Typography>
      <StateChip state={m.state} />
      <Typography variant="body2" color="text.secondary">
        {m.member ? `${m.role || "member"}${m.invitationPending ? " · invite pending" : ""}` : `not a member${m.live ? "" : " · not live here"}`}
        {" · teams: "}{cur.length ? cur.join(", ") : "none"}
      </Typography>
      {add.length > 0 && <Chip label={`+${add.join(", +")}`} color="success" variant="outlined" />}
      {del.length > 0 && <Chip label={`−${del.join(", −")}`} color="error" variant="outlined" />}
      {add.length === 0 && del.length === 0 && <Typography variant="body2" color="text.secondary">· in sync</Typography>}
    </Box>
  );
}

// PersonTraceBody is the full expanded trail for a mapped person.
export function PersonTraceBody({ p }: { p: Person }) {
  const missing = (p.expectedSources ?? []).filter((s) => !(p.sources ?? []).includes(s));
  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: 0.75, py: 1 }}>
      <IdPDirectories directories={p.directories} missing={missing} />
      {Object.entries(p.orgs ?? {}).map(([org, m]) => <OrgTrace key={org} org={org} m={m} />)}
      {p.noTeam && (
        <Typography variant="body2" color="text.secondary">
          Mapped, but no group/list/pin resolves to any team — nothing will be granted.
        </Typography>
      )}
    </Box>
  );
}
