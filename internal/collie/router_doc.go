package collie

import _ "embed"

// routerDoc is written to the router's working directory as ROUTER.md (and
// CLAUDE.md / AGENTS.md / GEMINI.md so every agent CLI loads it).
//
//go:embed templates/router.md
var routerDoc string

// routerWBDoc supplies the workbench instructions while the feature is on.
//
//go:embed templates/router_workbench.md
var routerWBDoc string
