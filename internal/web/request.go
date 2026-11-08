package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

// find returns a system the request may see (403 for a user without a role; 404 for unknown and hidden ids
// alike, so a viewer cannot tell that a hidden host exists).
func (c *reqCtx) find(id string) (model.System, error) {
	v, err := c.view()
	if err != nil {
		return model.System{}, err
	}
	return v.Find(id)
}

// decodeBody: strict JSON (unknown fields rejected), at most 64 KiB.
func (c *reqCtx) decodeBody(v any) error {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return apierr.New(400, "unreadable body")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apierr.New(422, "invalid request body")
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
		return 0, apierr.New(422, fmt.Sprintf("%s must be an integer between %d and %d", k, lo, hi))
	}
	return n, nil
}
