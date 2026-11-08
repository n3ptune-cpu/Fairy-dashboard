package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func (c *reqCtx) find(id string) (model.System, error) {
	for _, s := range c.s.Poller.Snapshot() {
		if s.ID == id {
			return s, nil
		}
	}
	return model.System{}, herr(404, "system not found")
}

// decodeBody: strict JSON (unknown fields rejected), at most 64 KiB.
func (c *reqCtx) decodeBody(v any) error {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return herr(400, "unreadable body")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return herr(422, "invalid request body")
	}
	return nil
}

func queryInt(q url.Values, k string, def, lo, hi int) (int, error) {
	v := q.Get(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, herr(422, fmt.Sprintf("%s must be an integer between %d and %d", k, lo, hi))
	}
	return n, nil
}
