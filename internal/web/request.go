package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
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

// decodeBody: strict JSON (unknown fields rejected), at most 64 KiB. On a route that can be kept for a
// step-up the body must also say each thing once (strictStepUpBody).
func (c *reqCtx) decodeBody(v any) error {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return apierr.New(400, "unreadable body")
	}
	if err := c.strictStepUpBody(b); err != nil {
		return err
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

// strictStepUpBody refuses, on the routes that can be kept for a step-up (actionText), a JSON body that does
// not say each thing once under its exact name (strictBody).
//
// Why (T-30): what "Confirm it's you" shows and what the step-up's audit entries say is the panel's own
// description of the kept request (describeAction), which reads the body's fields by their exact names. The
// handler decodes the same body into a struct, and that decoder takes a field under any spelling of its name
// ("ROLE" for "role") and, of a field given twice, the last one. With {"role":"pending","ROLE":"admin"} the
// dialog said "role: pending" and the panel made an admin. A body in which every name is exact and there
// once has one reading only, so the description is of what the handler runs.
func (c *reqCtx) strictStepUpBody(b []byte) error {
	if _, ok := actionText[c.r.Pattern]; ok && !strictBody(b) {
		return apierr.New(422, "invalid request body: a field is given twice or is not spelled exactly as the API "+
			"names it, or something follows the JSON value")
	}
	return nil
}

// apiFieldRe is how the API spells a field of a request body.
var apiFieldRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// strictBody reports whether a JSON body has one reading only: in no object are two names equal or equal
// except for the case of their letters (what encoding/json treats as the same field), the names of the
// outermost object are spelled like the API's fields (lower-case letters, digits, "_"), and nothing follows
// the value. An empty body, and a body that is not an object, pass: the handler decides about those.
func strictBody(b []byte) bool {
	if len(bytes.TrimSpace(b)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if !strictValue(dec, 0) {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

// strictValue reads one JSON value from dec and checks the names of every object in it (depth 0: the
// outermost value).
func strictValue(dec *json.Decoder, depth int) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return true // a text, a number, true, false, null
	}
	var names [][]byte
	for dec.More() {
		if d == '{' {
			kt, err := dec.Token()
			name, isName := kt.(string)
			if err != nil || !isName || (depth == 0 && !apiFieldRe.MatchString(name)) {
				return false
			}
			for _, n := range names {
				if bytes.EqualFold(n, []byte(name)) {
					return false
				}
			}
			names = append(names, []byte(name))
		}
		if !strictValue(dec, depth+1) {
			return false
		}
	}
	_, err = dec.Token() // the closing bracket
	return err == nil
}
