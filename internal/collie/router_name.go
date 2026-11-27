package collie

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// The Router's herdr name is routerName unless herdr would not give it that
// name at launch: another agent had it. Collie then keeps that agent if it is
// a Router that was alive all along (out of sight while its computer
// reconnected), or names the new one "collie-2", "collie-3", …. Everywhere
// else the Router is routerName: liveAgents maps whichever agent has its
// herdr name to it, and herdrTarget turns it back into that name.

// maxRouterNames bounds the spare names tried: collie-2 … collie-9.
const maxRouterNames = 9

// routerAgentLocked is the herdr name the Router was launched under.
func (sp *Super) routerAgentLocked() string {
	if n := sp.routerLaunchLocked().Name; n != "" {
		return n
	}
	return routerName
}

func (sp *Super) routerAgent() string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.routerAgentLocked()
}

// setRouterNameLocked records the herdr name a launch gave the Router.
func (sp *Super) setRouterNameLocked(name string) {
	l := sp.routerLaunchLocked()
	if name == routerName {
		name = ""
	}
	if l.Name != name {
		l.Name = name
		sp.saveRouterLaunchLocked(l)
	}
}

// herdrTarget is the herdr name that reaches target.
func (sp *Super) herdrTarget(target string) string {
	if target == routerName {
		return sp.routerAgent()
	}
	return target
}

// isRouterAgent reports whether a is a live Router named name on machine in cwd.
// Another agent (e.g. a dev Collie sharing herdr) with the same name in another
// folder or on another computer does not match.
func isRouterAgent(a Agent, name, machine, cwd string) bool {
	return deref(a.Name) == name && a.Agent != nil && cwd != "" && deref(a.Cwd) == cwd && machineOf(a.PaneID) == machine
}

func nameTaken(err error) bool {
	var he *HerdrError
	return errors.As(err, &he) && he.Code == "agent_name_taken"
}

// settleNameTaken runs when the Router just started in paneID (workspace
// wsID) could not be named routerName. If the agent with that name is a
// Router in the Router's folder on the same computer, it is the one Collie
// keeps: the new workspace is closed, so two Routers never take the same
// events. Otherwise the new agent gets the first spare name. It returns the
// name the Router now has.
func (sp *Super) settleNameTaken(ctx context.Context, machine, paneID, wsID, herdrKind string, args []string) (name string, kept bool, err error) {
	defer func() {
		var he *HerdrError
		if err != nil && !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": wsID}, nil) // no Router in it
		}
	}()
	agents, err := sp.s.herdr.Agents(ctx)
	if err != nil {
		return "", false, err
	}
	cwd := sp.routerCwd(machine)
	taken := map[string]bool{}
	for _, a := range agents {
		n := deref(a.Name)
		taken[n] = true
		if a.PaneID != paneID && isRouterAgent(a, routerName, machine, cwd) {
			if err := sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": wsID}, nil); err != nil {
				return "", false, fmt.Errorf("Collie 已经在 %s 运行，但没能关掉多起的那个：%w", a.PaneID, err)
			}
			return routerName, true, nil
		}
	}
	for i := 2; i <= maxRouterNames; i++ {
		name = fmt.Sprintf("%s-%d", routerName, i)
		if taken[name] {
			continue
		}
		// Looked at each time: a refused agent.start may have left the agent running.
		p, err := sp.s.herdr.Pane(ctx, paneID)
		if err != nil {
			return "", false, err
		}
		if p.Agent != nil {
			err = sp.s.herdr.Call(ctx, "agent.rename", map[string]any{"target": paneID, "name": name}, nil)
		} else {
			err = sp.s.herdrStart(ctx, paneID, name, herdrKind, args)
		}
		if nameTaken(err) {
			continue
		}
		sp.s.noteStarted(paneID, name, err)
		log.Printf("router: %q is taken by another agent; the Router is %q", routerName, name)
		return name, false, err
	}
	return "", false, fmt.Errorf("%s 和 %s-2 … %s-%d 都被别的 agent 占用了", routerName, routerName, routerName, maxRouterNames)
}
