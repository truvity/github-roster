import { createTheme } from "@mui/material/styles";

// The console's Material theme. Teal primary keeps continuity with the
// pre-MUI palette; density leans compact because every view is a table.
export const theme = createTheme({
  palette: {
    primary: { main: "#0f766e" },
    background: { default: "#fafafa" },
  },
  typography: {
    fontSize: 13.5,
    h1: { fontSize: "1.6rem", fontWeight: 600 },
    h2: { fontSize: "1.2rem", fontWeight: 600 },
    h3: { fontSize: "1.05rem", fontWeight: 600 },
  },
  components: {
    MuiTableCell: {
      styleOverrides: { root: { paddingTop: 8, paddingBottom: 8 } },
    },
    MuiChip: {
      defaultProps: { size: "small" },
    },
    MuiButton: {
      defaultProps: { size: "small" },
    },
    MuiTextField: {
      defaultProps: { size: "small" },
    },
  },
});

// stateColor maps a lifecycle state to a Chip color.
export function stateColor(state?: string): "success" | "warning" | "error" | "default" {
  switch (state) {
    case "synced": return "success";
    case "pending": case "invited": return "warning";
    case "leaving": case "unknown": return "error";
    case "display-only": return "default";
    default: return "default";
  }
}
