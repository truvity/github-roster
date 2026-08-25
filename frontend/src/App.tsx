import { useEffect, useState } from "react";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Container from "@mui/material/Container";
import Tab from "@mui/material/Tab";
import Tabs from "@mui/material/Tabs";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import { fetchMe, fetchVersion } from "./api";
import { useAsync } from "./hooks";
import { PeopleView } from "./PeopleView";
import { StatusView } from "./StatusView";
import { HistoryView } from "./HistoryView";
import { SettingsView } from "./SettingsView";

type TabKey = "people" | "status" | "history" | "settings";

const TABS: TabKey[] = ["people", "status", "history", "settings"];

// tabFromHash reads the active tab from the URL fragment (#status, …) so a
// refresh, the back button, and shared deep links all land on the same view.
// An unknown or empty fragment falls back to People.
function tabFromHash(): TabKey {
  const h = (typeof location !== "undefined" ? location.hash.replace(/^#/, "") : "") as TabKey;
  return TABS.includes(h) ? h : "people";
}

// VersionBadge shows the running build, fetched over ConnectRPC.
function VersionBadge() {
  const { data } = useAsync(fetchVersion);
  if (!data) return null;
  const commit = data.commit ? ` (${data.commit.slice(0, 7)})` : "";
  return <Typography variant="caption" color="text.secondary" title="via ConnectRPC">{data.version}{commit}</Typography>;
}

// UserBadge shows the signed-in caller and role, with a sign-out link. Sign
// out goes to the oauth2-proxy gateway endpoint, which clears the session
// cookie; the next request re-runs the OIDC login.
function UserBadge() {
  const { data } = useAsync(fetchMe);
  if (!data) return null;
  const name = data.name || data.email || "signed in";
  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 1.5 }}>
      <Box sx={{ textAlign: "right", lineHeight: 1.15 }}>
        <Typography variant="body2">{name}</Typography>
        {data.name && data.email && <Typography variant="caption" color="text.secondary">{data.email}</Typography>}
      </Box>
      {data.role && <Chip label={data.role} color={data.role === "operator" ? "success" : "default"} variant="outlined" />}
      <Button href="/oauth2/sign_out" variant="outlined" title="Clear the session and sign out">Sign out</Button>
    </Box>
  );
}

export function App() {
  const [tab, setTab] = useState<TabKey>(tabFromHash);
  // Keep state in sync with the URL fragment so the back/forward buttons and
  // deep links work; the nav tabs are plain anchors (#status) that set it.
  useEffect(() => {
    const onHash = () => setTab(tabFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const labels: Record<TabKey, string> = { people: "People", status: "Status", history: "History", settings: "Settings" };
  return (
    <>
      <AppBar position="sticky" color="default" elevation={0} sx={{ borderBottom: 1, borderColor: "divider", bgcolor: "background.paper" }}>
        <Toolbar variant="dense" sx={{ gap: 2 }}>
          <Typography variant="h3" component="span" sx={{ fontWeight: 700 }}>roster</Typography>
          <Tabs value={tab} sx={{ minHeight: 48 }}>
            {TABS.map((k) => <Tab key={k} value={k} label={labels[k]} component="a" href={`#${k}`} sx={{ minHeight: 48 }} />)}
          </Tabs>
          <Box sx={{ ml: "auto" }}><UserBadge /></Box>
        </Toolbar>
      </AppBar>
      <Container maxWidth="lg" sx={{ py: 3 }}>
        <Typography variant="h1" sx={{ mb: 2 }}>{labels[tab]}</Typography>
        {tab === "people" && <PeopleView />}
        {tab === "status" && <StatusView />}
        {tab === "history" && <HistoryView />}
        {tab === "settings" && <SettingsView />}
        <Box component="footer" sx={{ mt: 6, pt: 2, borderTop: 1, borderColor: "divider", textAlign: "center" }}>
          <VersionBadge />
        </Box>
      </Container>
    </>
  );
}
