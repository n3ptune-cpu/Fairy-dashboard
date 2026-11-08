package web

import (
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

// demoViews are the roles the demo can preview: admin, a viewer of each site, and a pending user.
func (s *Server) demoViews() []string {
	out := []string{access.RoleAdmin}
	for _, site := range s.Access.Load().Sites {
		out = append(out, access.RoleViewer+":"+site)
	}
	return append(out, access.RolePending)
}

// demoRoutes: demo mode only (loopback, no login). POST /api/demo/view {"view": "viewer:Straw"} switches what
// the demo shows, so the owner can check what a viewer of one site sees.
func (s *Server) demoRoutes() {
	s.mux.HandleFunc("POST /api/demo/view", s.h(func(c *reqCtx) error {
		u, err := c.requireUser()
		if err != nil {
			return err
		}
		if err := c.checkCSRF(u); err != nil {
			return err
		}
		var body struct {
			View string `json:"view"`
		}
		if err := c.decodeBody(&body); err != nil {
			return err
		}
		if err := s.SetDemoView(body.View); err != nil {
			return apierr.New(422, err.Error())
		}
		c.json(200, map[string]any{"view": body.View})
		return nil
	}))
}
