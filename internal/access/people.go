package access

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// How the panel names a person.
//
// A person is named by the user name of their account (the OIDC claim preferred_username), never by the e-mail
// address or the name the account gives for the person. The address and that name are for an admin only: the
// Users page and the audit log have them, and nothing else the panel sends, writes into a sentence, tells an
// agent or pushes to a notification service.
//
// Older versions named a person by the address (or, without one, by the name), and that text is in what they
// stored: who added a host, who put a machine id on record, who started a run. Those records are not rewritten.
// A stored text is turned into the user name where it is handed out (Shown, ShownIn), by looking it up among
// the accounts of users.json. Only one account may match: an address or a name that two known accounts have
// says nothing about which of them it was.
//
// None of this decides anything. Roles, sites, allowed_emails and bootstrap_admins never read the user name:
// like the name and the address it is what the account says about itself, and only the subject is certain.

const (
	// HiddenAddress and HiddenName stand where a text names a person by an address or a name and the panel has
	// no user name to put there: the account has none (it has not signed in since the panel keeps user names,
	// or its provider sends none), the account is not known any more, or more than one account matches. The
	// words say why nothing is there; they say nothing about the account.
	HiddenAddress = "(address hidden)"
	HiddenName    = "(name hidden)"

	// UnverifiedMark follows an e-mail address the identity provider did not mark as verified, where the panel
	// names an account by it (audit entries, and the records of older versions).
	UnverifiedMark = " (unverified)"

	// MaxUsername is how many bytes of a user name are kept.
	MaxUsername = 64
)

// CleanUsername is a user name as the panel keeps it: without control characters, escape sequences, line
// breaks and direction overrides (textsafe.Clean), without spaces at either end, and at most MaxUsername
// bytes. The claim is whatever the account's provider sent.
func CleanUsername(s string) string { return textsafe.Cut(s, MaxUsername) }

// People answers how the accounts of one users.json are named. It is made once for an answer (Data.People).
type People struct {
	users  map[string]User
	byText map[string][]string // an address, a name or a user name in small letters -> the subjects that have it
	shown  map[string]string   // subject -> the user name the account is shown by ("" = none)
	names  []string            // the names of accounts without an address, longest first (ShownIn)
}

func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// People indexes the accounts.
func (d Data) People() *People {
	p := &People{users: d.Users, byText: map[string][]string{}, shown: map[string]string{}}
	add := func(text, sub string) {
		k := fold(text)
		if k == "" || containsStr(p.byText[k], sub) {
			return
		}
		p.byText[k] = append(p.byText[k], sub)
	}
	for sub, u := range d.Users {
		add(u.Email, sub)
		add(u.Name, sub)
		add(u.Username, sub)
		if u.Email == "" && len(u.Name) > 1 {
			p.names = append(p.names, u.Name)
		}
	}
	sort.Slice(p.names, func(i, j int) bool {
		if len(p.names[i]) != len(p.names[j]) {
			return len(p.names[i]) > len(p.names[j])
		}
		return p.names[i] < p.names[j]
	})
	for sub, u := range d.Users {
		// A user name is shown when it is the account's alone: no other account has it as its user name, its
		// address or its name, and it is no other account's subject. (An account could otherwise call itself
		// by somebody else's address.) A user name that is the account's own address is the address.
		name := u.Username
		if name == "" || fold(name) == fold(u.Email) || len(p.byText[fold(name)]) != 1 {
			continue
		}
		if other, ok := d.Users[name]; ok && other.Sub != sub {
			continue
		}
		p.shown[sub] = name
	}
	return p
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// match returns the accounts a text names: the one with that subject, and those that have it as their
// address, name or user name.
func (p *People) match(text string) []string {
	subs := append([]string(nil), p.byText[fold(text)]...)
	if _, ok := p.users[text]; ok && !containsStr(subs, text) {
		subs = append(subs, text)
	}
	return subs
}

// Username is the user name the account with this subject is shown by; "" when it has none that can be shown.
func (p *People) Username(sub string) string { return p.shown[sub] }

// Of is how the account with this subject is named: by its user name; without one, by the words that stand
// for the address or the name it would have been named by before (never by them), and an account that has
// neither by its subject, as before. ok is false when no such account is known.
func (p *People) Of(sub string) (name string, ok bool) {
	u, ok := p.users[sub]
	if !ok {
		return "", false
	}
	if n := p.shown[sub]; n != "" {
		return n, true
	}
	switch {
	case u.Email != "":
		return HiddenAddress, true
	case u.Name != "":
		return HiddenName, true
	case u.Username != "": // not the account's alone: no name, and not somebody else's
		return HiddenName, true
	}
	return sub, true
}

// Record is what a record keeps for something the account with this subject did: its user name, or, while it
// has none, the text older versions kept (stored), which Shown hides where the record is handed out and turns
// into the user name once the account has one.
func (p *People) Record(sub, stored string) string {
	if n := p.shown[sub]; n != "" {
		return n
	}
	return stored
}

// Shown is a stored text that names a person, as it is handed out anywhere but on the Users page and in the
// audit log. It is the one place that decides it:
//
//   - the text is the address, the name, the user name or the subject of exactly one known account: that
//     account's user name; when it has none, HiddenAddress or HiddenName (a subject stays, as before);
//   - more than one account matches: HiddenAddress or HiddenName (a text that is only a user name stays);
//   - an API token ("ci (token, owner <subject>)"): the same, with the owner named like an account;
//   - anything else is not an account's text ("root" from the command line, "fairy"): it stays, with every
//     address in it handled as in ShownIn. What still holds an "@" after that is HiddenAddress as a whole:
//     the field names a person, and an "@" in it is an address or a part of one.
func (p *People) Shown(stored string) string {
	if stored == "" {
		return ""
	}
	base := strings.TrimSuffix(stored, UnverifiedMark)
	if i := strings.LastIndex(base, tokenOwner); i > 0 && strings.HasSuffix(base, ")") {
		owner := base[i+len(tokenOwner) : len(base)-1]
		if n, ok := p.Of(owner); ok {
			owner = n
		} else if strings.Contains(owner, "@") {
			owner = HiddenAddress
		}
		name := base[:i]
		if strings.Contains(name, "@") {
			name = p.ShownIn(name)
		}
		return name + tokenOwner + owner + ")"
	}
	subs := p.match(base)
	if len(subs) == 1 {
		u := p.users[subs[0]]
		if n := p.shown[u.Sub]; n != "" {
			return n
		}
		switch {
		case base == u.Sub:
			return base
		case strings.Contains(base, "@") || fold(base) == fold(u.Email):
			return HiddenAddress
		}
		return HiddenName
	}
	if len(subs) > 1 {
		private := strings.Contains(base, "@")
		named := false
		for _, sub := range subs {
			u := p.users[sub]
			private = private || fold(base) == fold(u.Email)
			named = named || fold(base) == fold(u.Name)
		}
		switch {
		case private:
			return HiddenAddress
		case named:
			return HiddenName
		}
		return base
	}
	out := p.ShownIn(stored)
	if strings.Contains(strings.ReplaceAll(out, HiddenAddress, ""), "@") && !p.onlyUsernames(out) {
		return HiddenAddress
	}
	return out
}

// tokenOwner is the middle of the text an API token is named by: "<name> (token, owner <subject>)".
const tokenOwner = " (token, owner "

// onlyUsernames reports whether every "@" of a text ShownIn made belongs to a user name it put there (a user
// name may hold one).
func (p *People) onlyUsernames(out string) bool {
	for _, n := range p.shown {
		if strings.Contains(n, "@") {
			out = strings.ReplaceAll(out, n, "")
		}
	}
	return !strings.Contains(strings.ReplaceAll(out, HiddenAddress, ""), "@")
}

// ShownIn is a sentence older versions made with a person in it ("started by a@example.com: 3 machines"), as
// it is handed out anywhere but on the Users page and in the audit log. Every e-mail address in it is replaced:
// by the user name of the one known account that has it, else by HiddenAddress; the mark for an address that
// was not verified goes with it. The name of a known account that has no address (the only accounts older
// versions named by their name) is replaced the same way where it stands as a word of its own.
//
// An address here is what looks like one: text around an "@" whose right side has a dot and ends in letters.
// "root@pam" and "root@192.0.2.7" in an error text are left alone. The rest of the sentence is not touched.
func (p *People) ShownIn(text string) string {
	if strings.Contains(text, "@") {
		text = p.addresses(text)
	}
	for _, name := range p.names {
		text = p.words(text, name)
	}
	return text
}

func localRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._%+-'", r)
}

func domainRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-'
}

func (p *People) addresses(text string) string {
	var b strings.Builder
	done := 0 // text[:done] is written
	for at := strings.IndexByte(text, '@'); at >= 0; {
		l := at
		for l > done {
			r, n := utf8.DecodeLastRuneInString(text[done:l])
			if !localRune(r) {
				break
			}
			l -= n
		}
		r := at + 1
		for r < len(text) {
			c, n := utf8.DecodeRuneInString(text[r:])
			if !domainRune(c) {
				break
			}
			r += n
		}
		for r > at+1 && (text[r-1] == '.' || text[r-1] == '-') { // the full stop of the sentence
			r--
		}
		next := r
		if isAddress(text[l:at], text[at+1:r]) {
			b.WriteString(text[done:l])
			b.WriteString(p.address(text[l:r]))
			for _, mark := range []string{UnverifiedMark, agentMark} { // the mark was about the address
				if strings.HasPrefix(text[r:], mark) {
					next = r + len(mark)
					break
				}
			}
			done = next
		}
		i := strings.IndexByte(text[next:], '@')
		if i < 0 {
			break
		}
		at = next + i
	}
	b.WriteString(text[done:])
	return b.String()
}

// agentMark is UnverifiedMark as an agent repeats it in "requested by": it keeps letters, digits, spaces and
// . _ @ - of what it was told, so the brackets are gone.
const agentMark = " unverified"

// isAddress: something before the "@", and after it a name with a dot that ends in two or more letters.
func isAddress(local, domain string) bool {
	i := strings.LastIndexByte(domain, '.')
	if local == "" || i <= 0 || len(domain)-i-1 < 2 {
		return false
	}
	for _, r := range domain[i+1:] {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// address is what stands for an e-mail address in a sentence.
func (p *People) address(addr string) string {
	if subs := p.byText[fold(addr)]; len(subs) == 1 {
		if n := p.shown[subs[0]]; n != "" {
			return n
		}
	}
	return HiddenAddress
}

// words replaces name in text where it stands as a word of its own.
func (p *People) words(text, name string) string {
	if !strings.Contains(text, name) {
		return text
	}
	repl := HiddenName
	if subs := p.byText[fold(name)]; len(subs) == 1 && p.shown[subs[0]] != "" {
		repl = p.shown[subs[0]]
	}
	edge := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	var b strings.Builder
	for {
		i := strings.Index(text, name)
		if i < 0 {
			break
		}
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[i+len(name):])
		b.WriteString(text[:i])
		if (i == 0 || edge(before)) && (i+len(name) == len(text) || edge(after)) {
			b.WriteString(repl)
		} else {
			b.WriteString(name)
		}
		text = text[i+len(name):]
	}
	b.WriteString(text)
	return b.String()
}

// Private are the addresses and names of the known accounts, longest first: what the Users page and the audit
// log draw as dots until it is clicked. For admins only.
func (p *People) Private() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, u := range p.users {
		for _, v := range []string{u.Email, u.Name} {
			if v != "" && !seen[v] && fold(v) != fold(p.shown[u.Sub]) { // (a name that is the user name is no secret)
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// Named are the subjects of the accounts whose shown user name holds text (in small letters): what a search for
// a person by the name the panel shows finds, also in records that only have the subject.
func (p *People) Named(text string) []string {
	text = fold(text)
	var subs []string
	if text == "" {
		return subs
	}
	for sub, n := range p.shown {
		if strings.Contains(strings.ToLower(n), text) {
			subs = append(subs, sub)
		}
	}
	sort.Strings(subs)
	return subs
}
