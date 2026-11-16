package access

import (
	"sort"
	"strings"
	"time"
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
// says nothing about which of them it was. A text that is what an account is shown by is that account's alone
// (it is what a record keeps since build 17). And only the accounts that have a role are looked at: an account
// that only waits for access wrote no record, and what it calls itself (a name, an address, a user name) is
// read out of no stored text and changes how no record of another account reads. Its address is still found in
// a sentence and hidden there, like every address.
//
// A user name is what the account says, and two accounts can say the same. Only user names compete with user
// names: an account whose display name or address is somebody's user name takes nothing away from that
// account. Two user names are the same when their folded forms are (fold: letters without their case, no
// characters that draw nothing such as a zero width space, one space for many). Of the accounts that have one
// user name, one is shown by it:
// an admin before a viewer, a viewer before an account that only waits for access, and among equals the one
// that has had the user name longest (User.UsernameSince; the account's creation for a user name from before
// that field), then the smaller account id.
// Every other one is shown by the user name with a short form of its account id behind it ("Juniper · id
// 3f6c1d"), everywhere, never by the plain name: so an account without any access cannot take an admin's name
// away, and nobody is left without a name. A user name that is written like a text of the panel's own (with
// the "·" of that mark, with a bracket like a token's text, or one of the panel's words such as "root") is
// shown with its account's id in every case (marked), so that no account is shown by the text of another
// account, of a token, of the command line or of the panel.
//
// A text that somebody outside the panel supplies as the name of a person (an agent's "requested by") is never
// looked up among addresses and names: that would tell whoever runs the agent which user name belongs to an
// address (FromOutside).
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

	// idMark stands between a user name and the short form of the account's id, for an account that is not the
	// one shown by the plain user name. shortID is how many characters of the id that form has.
	idMark  = " · id "
	shortID = 6
)

// The reasons a user name an account has is not what it is shown by (People.UsernameNote).
const (
	UsernameShared    = "same_username" // another account has the same user name: all but one are shown with a part of their id
	UsernameIsAddress = "is_address"    // it is an e-mail address, the account's own or another account's
	UsernameIsAccount = "is_account_id" // it is another account's id
	UsernameMarked    = "panel_mark"    // it is written like a text of the panel's own (marked): shown with a part of the account's id, never plain
)

// ownWords are the words the panel itself shows in the place of who did something when it was no account, or
// none it names, in their folded forms: the command line's actor, the panel, a token of nobody, what an agent
// says when it was told nothing, and the words for an account without a user name as an agent repeats them
// (without their brackets; with them they are marked as every text with a bracket is). A user name that is
// one of them is never shown plain (marked).
var ownWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range []string{"root", "fairy", outsideNobody, "system", "This account", "the account", "this user", "address hidden", "name hidden"} {
		m[fold(w)] = true
	}
	return m
}()

// marked reports whether a user name is written like a text of the panel's own:
//
//   - it has a "·", the mark between a user name and an account id ("Juniper · id 3f6c1d" as somebody's own
//     user name, with the admin's id, read like the panel's way of saying "the account whose id begins 3f6c1d");
//   - it has a bracket, as the text of an API token has ("ci (token, owner Juniper)" was the same text as the
//     admin's token "ci"), the command line's "root (cli)", and the words "(address hidden)";
//   - it is one of the panel's own words (ownWords), in any case of its letters.
//
// An account with such a user name is shown by it with a short form of its own id behind it, whatever its role
// and whether another account has the name or not (People). So what an account is shown by either has no "·"
// and no bracket and is none of those words, or ends in the mark and that account's own id: it is never the
// text of another account, of a token (which ends in a bracket) or of the panel.
func marked(username string) bool {
	return strings.ContainsAny(username, "·()") || ownWords[fold(username)]
}

// CleanUsername is a user name as the panel keeps it: without control characters, escape sequences, line
// breaks and direction overrides (textsafe.Clean), without the characters that draw nothing (blank: a name
// with one of them in it looks like the name without), with one plain space wherever it has spaces of any
// kind (oneSpace), without spaces at either end, and at most MaxUsername bytes. The claim is whatever the
// account's provider sent. Both places that take a user name in call this: where the claims are read, and
// where users.json is written.
func CleanUsername(s string) string {
	return textsafe.Cut(oneSpace(noBlank(textsafe.Clean(s))), MaxUsername)
}

// blank reports the characters that draw nothing and take no room, or draw an empty place that is no space:
//
//   - the format characters (Unicode category Cf): the zero width space and joiners, the word joiner, the soft
//     hyphen, the byte order mark, the Mongolian vowel separator;
//   - the variation selectors (U+FE00 to U+FE0F, U+E0100 to U+E01EF, the Mongolian U+180B to U+180D and
//     U+180F): they choose the form of the character before them, and after a plain letter they do nothing;
//   - every other character with the Unicode property Default_Ignorable_Code_Point (the standard library has
//     it as Other_Default_Ignorable_Code_Point, Cf and Variation_Selector): the combining grapheme joiner
//     U+034F, the Hangul fillers U+115F, U+1160, U+3164 and U+FFA0, the Khmer inherent vowels, the tag
//     characters and the code points kept free for more of them;
//   - the Braille pattern without dots, U+2800: a character of its own that is drawn as an empty cell.
//
// Until the fix round after the fifth review only Cf was removed, and "Juniper" with a variation selector or a
// Hangul filler behind it was shown plain next to the admin Juniper.
func blank(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) || r == 0x2800
}

// noBlank is s without the characters that draw nothing (blank).
func noBlank(s string) string {
	return strings.Map(func(r rune) rune {
		if blank(r) {
			return -1
		}
		return r
	}, s)
}

// oneSpace is s with one plain space in the place of every run of spaces, of any kind (the Unicode category Zs:
// the no-break space, the em space, the ideographic space; and tabs and line ends), and none at either end. A
// page draws two spaces like one, and an em space like a space: "Pine  Top" was another user name than
// "Pine Top" and looked the same.
func oneSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// People answers how the accounts of one users.json are named. It is made once for an answer (Data.People).
type People struct {
	users map[string]User
	// What a stored text can mean (match), each by the folded text: the accounts that have it as their address;
	// those that older versions named by it (the name of an account without an address) and those that still
	// write it into a record (a user name that is not shown, of an account without an address); and the
	// accounts that have it as their name next to an address, which no record was ever written with.
	byAddr, byName, byNameToo map[string][]string
	shown                     map[string]string   // subject -> what the account is shown by: its user name, or that with its id ("" = none)
	byShown                   map[string]string   // a shown form, folded -> the subject it is the form of (accounts with a role)
	same                      map[string][]string // subject -> the other accounts that have the same user name, sorted
	note                      map[string]string   // subject -> why its user name is not what it is shown by (UsernameNote)
	names                     []string            // the names of accounts without an address, longest first (ShownIn)
	addrs                     []string            // the addresses of the accounts (those with an "@"), longest first (ShownIn)
	outside                   map[string][]string // a shown form as an agent repeats it (outsideForm) -> the subjects shown by it (accounts with a role)
}

// fold is the form two texts are compared in: without the characters that draw nothing (blank: a zero width
// space, a variation selector and the like), with one plain space for every run of spaces of any kind and none
// at either end (oneSpace), and every letter by its simple case fold, so that "JUNIPER", "juniper" and
// "Juniper" with a zero width joiner in it are one text. (Composed and decomposed letters, and look-alike
// letters of other alphabets, stay different texts: the account id tells such accounts apart.)
func fold(s string) string {
	return oneSpace(strings.Map(func(r rune) rune {
		if blank(r) {
			return -1
		}
		low := r // the smallest letter of those that fold to the same
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			low = min(low, f)
		}
		return low
	}, s))
}

// usernameSince is since when the account has had its user name: the stored time, and for a user name from
// before the panel kept that time the account's creation.
func (u User) usernameSince() time.Time {
	if u.UsernameSince != nil {
		return *u.UsernameSince
	}
	return u.CreatedAt
}

// firstOf reports whether a comes before b among the accounts that have one user name: an admin before a
// viewer, a viewer before an account that only waits (rank); then the one that has had the user name longer;
// then by subject (so that the answer is the same every time).
//
// The time alone does not decide between an admin and a viewer. It is the sign-in that brought the user name,
// and at the first start of this build, and after build 16 wrote users.json (it drops the user name and the
// time), that is whose browser asks first: a viewer who had set the admin's user name at the provider was the
// plain "Juniper" from then on, and the admin "Juniper · id 3f6c1d".
func firstOf(a, b User) bool {
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	if ta, tb := a.usernameSince(), b.usernameSince(); !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.Sub < b.Sub
}

// rank orders the accounts that want one user name by what they may do: an admin, a viewer, any other account
// (one that waits for access).
func rank(u User) int {
	switch u.Role {
	case RoleAdmin:
		return 0
	case RoleViewer:
		return 1
	}
	return 2
}

// People indexes the accounts.
func (d Data) People() *People {
	p := &People{users: d.Users, byAddr: map[string][]string{}, byName: map[string][]string{}, byNameToo: map[string][]string{},
		shown: map[string]string{}, byShown: map[string]string{}, same: map[string][]string{}, note: map[string]string{},
		outside: map[string][]string{}}
	add := func(index map[string][]string, text, sub string) {
		if k := fold(text); k != "" && !containsStr(index[k], sub) {
			index[k] = append(index[k], sub)
		}
	}
	found := map[string]bool{} // the addresses that are in addrs, folded
	for sub, u := range d.Users {
		// Every account's address is found in a sentence and hidden there (addrs), whoever has it.
		if k := fold(u.Email); strings.Contains(k, "@") && !found[k] {
			found[k] = true
			p.addrs = append(p.addrs, u.Email)
		}
		// An account that only waits for access takes part in nothing else: no stored text is read as its
		// address, its name or its user name. It wrote no record (it cannot act), what it calls itself is its
		// own choice, and it must not be the reason a record of another account reads differently: with the
		// name "Juniper" and no address it made every record by the admin Juniper "(name hidden)".
		if u.Role == RolePending {
			continue
		}
		add(p.byAddr, u.Email, sub)
		if u.Email == "" {
			add(p.byName, u.Name, sub)
			if len(u.Name) > 1 {
				p.names = append(p.names, u.Name)
			}
		} else {
			add(p.byNameToo, u.Name, sub)
		}
	}
	longestFirst(p.names)
	longestFirst(p.addrs)
	// The user names. Only user names compete with user names: what another account has as its name or its
	// address takes no user name away. Three user names are no name for anybody, whoever has them: one that is
	// the account's own address (it would be the address), one that is written like an address and is another
	// account's (the panel never names a person by an address), and one that is another account's id.
	groups := map[string][]string{} // a user name, folded -> the subjects that have it
	for sub, u := range d.Users {
		name := CleanUsername(u.Username)
		k := fold(name)
		switch other, isID := d.Users[name]; {
		case k == "":
		case k == fold(u.Email), strings.Contains(k, "@") && len(p.byAddr[k]) > 0:
			p.note[sub] = UsernameIsAddress
		case isID && other.Sub != sub:
			p.note[sub] = UsernameIsAccount
		default:
			groups[k] = append(groups[k], sub)
		}
	}
	for _, subs := range groups {
		sort.Slice(subs, func(i, j int) bool { return firstOf(d.Users[subs[i]], d.Users[subs[j]]) })
		for i, sub := range subs {
			name := CleanUsername(d.Users[sub].Username)
			own := marked(name) // (the same for all of subs: they are one text in the folded form)
			if i > 0 || own {
				name = withID(name, sub, subs)
			}
			p.shown[sub] = name
			if own {
				p.note[sub] = UsernameMarked
			}
			if len(subs) > 1 {
				if !own {
					p.note[sub] = UsernameShared
				}
				for _, other := range subs {
					if other != sub {
						p.same[sub] = append(p.same[sub], other)
					}
				}
				sort.Strings(p.same[sub])
			}
		}
	}
	p.makeUnique()
	for sub, u := range d.Users {
		// An account without an address and without a user name the panel shows is still written into a record
		// by that user name (the text the audit log names it by): a stored text can mean it.
		if p.shown[sub] == "" && u.Email == "" && u.Role != RolePending {
			add(p.byName, CleanUsername(u.Username), sub)
		}
	}
	for sub, n := range p.shown {
		// (what an account that only waits is shown by is read out of no record and no agent's answer: it
		// chose the text, and it could choose the name a record of another account was written with)
		if d.Users[sub].Role == RolePending {
			continue
		}
		p.byShown[fold(n)] = sub
		if k := outsideForm(n); k != "" {
			p.outside[k] = append(p.outside[k], sub)
		}
	}
	return p
}

func longestFirst(l []string) {
	sort.Slice(l, func(i, j int) bool {
		if len(l[i]) != len(l[j]) {
			return len(l[i]) > len(l[j])
		}
		return l[i] < l[j]
	})
}

// withID is a user name with the short form of the account's id behind it. The short form is the first shortID
// characters of the id, and the whole id when another of the accounts with that user name (group) begins the
// same.
func withID(name, sub string, group []string) string {
	short := sub
	if r := []rune(sub); len(r) > shortID {
		short = string(r[:shortID])
	}
	for _, other := range group {
		if other != sub && strings.HasPrefix(other, short) {
			short = sub
			break
		}
	}
	return name + idMark + short
}

// makeUnique sees to it that no two accounts are shown by the same text. A user name can be written like
// another account's user name with an id behind it ("Juniper · id 3f6c1d" as somebody's own user name): the
// account whose own user name that is gets its id behind it too. What is still the same after that (it takes
// user names made for it) is shown by nothing.
func (p *People) makeUnique() {
	for round := 0; ; round++ {
		byForm := map[string][]string{}
		for sub, n := range p.shown {
			byForm[fold(n)] = append(byForm[fold(n)], sub)
		}
		again := false
		for _, subs := range byForm {
			if len(subs) < 2 {
				continue
			}
			again = true
			for _, sub := range subs {
				p.note[sub] = UsernameShared
				if round == 3 {
					delete(p.shown, sub)
				} else if p.shown[sub] == CleanUsername(p.users[sub].Username) {
					p.shown[sub] = withID(p.shown[sub], sub, nil)
				}
			}
		}
		if !again {
			return
		}
	}
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// match returns the accounts a stored text can mean, and whether one of them has it as its address, or as the
// name (or the user name that is not shown) a record was written with:
//
//   - the account that is shown by it (what a record keeps since build 17: the user name, or the user name
//     with the account's id behind it), and then that account alone: no other account's name or address
//     makes such a text "two accounts" (an account without an address whose name was an admin's user name
//     made every record of that admin "(name hidden)");
//   - else the accounts that have it as their address; the accounts without an address that have it as their
//     name (older versions named those by it) or as a user name the panel does not show (a record is still
//     written with it);
//   - the account with that subject;
//   - and only when it is none of these: the accounts that have it as their name next to an address. No record
//     was written with such a name, so it stands in nobody's way; it is looked up so that it is never handed
//     out as it is.
//
// A user name that an account merely has, while another account is shown by it, is not among them: only user
// names compete with user names, and the account that is shown by the plain name is the one a record means.
//
// An account that only waits for access is among them by what it is shown by and by its subject, and by
// nothing else (People).
func (p *People) match(text string) (subs []string, address, name bool) {
	k := fold(text)
	if sub, ok := p.byShown[k]; ok {
		return []string{sub}, false, false
	}
	add := func(l ...string) {
		for _, sub := range l {
			if !containsStr(subs, sub) {
				subs = append(subs, sub)
			}
		}
	}
	add(p.byAddr[k]...)
	address = len(subs) > 0
	add(p.byName[k]...)
	name = len(subs) > len(p.byAddr[k])
	if sub := p.withIDOf(text); sub != "" {
		add(sub)
	}
	if _, ok := p.users[text]; ok {
		add(text)
	}
	if len(subs) == 0 {
		add(p.byNameToo[k]...)
		name = len(subs) > 0
	}
	return subs, address, name
}

// withIDOf is the account a text of the form "<user name> · id <short id>" names, when no account is shown by
// that text right now: a record kept it while two accounts had the user name, and the account is shown by the
// plain name since (or by its whole id behind it). "" when no account or more than one fits.
func (p *People) withIDOf(text string) string {
	i := strings.LastIndex(text, idMark)
	if i <= 0 {
		return ""
	}
	name, short, found := fold(text[:i]), text[i+len(idMark):], ""
	for sub, u := range p.users {
		if p.shown[sub] == "" || fold(CleanUsername(u.Username)) != name {
			continue
		}
		if sub != short && withID("", sub, nil) != idMark+short {
			continue
		}
		if found != "" {
			return ""
		}
		found = sub
	}
	return found
}

// Username is what the account with this subject is shown by: its user name, or its user name with a short
// form of its id behind it while another account that comes first has the same; "" when it has none that can
// be shown.
func (p *People) Username(sub string) string { return p.shown[sub] }

// SameUsername are the subjects of the other accounts that have the same user name as this one (in its folded
// form), sorted; nil when no other account has it.
func (p *People) SameUsername(sub string) []string { return p.same[sub] }

// UsernameNote is why the user name of this account is not simply what it is shown by: UsernameShared (another
// account has the same; this one or the other is shown with a part of its id), UsernameMarked (it is written
// like a text of the panel's own, and shown with a part of the account's id), UsernameIsAddress,
// UsernameIsAccount (it is shown by no user name), or "" (it is shown by its user name, or has none).
func (p *People) UsernameNote(sub string) string { return p.note[sub] }

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
	case u.Username != "": // an address, or somebody else's id: no name
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
//   - the text is the address, the name or the subject of exactly one known account, or what one account is
//     shown by (its user name; with its id behind it for an account that shares the user name): what that
//     account is shown by; when it has no user name, HiddenAddress or HiddenName (a subject stays, as before);
//   - more than one account matches: HiddenAddress or HiddenName;
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
	subs, address, named := p.match(base)
	address = address || strings.Contains(base, "@")
	if len(subs) == 1 {
		u := p.users[subs[0]]
		if n := p.shown[u.Sub]; n != "" {
			return n
		}
		switch {
		case base == u.Sub:
			return base
		case address:
			return HiddenAddress
		}
		return HiddenName
	}
	if len(subs) > 1 {
		switch {
		case address:
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
// by what the one known account that has it is shown by, else by HiddenAddress; the mark for an address that
// was not verified goes with it. The name of a known account that has no address (the only accounts older
// versions named by their name) is replaced the same way where the sentence names who did something (words).
//
// An address here is, first, the address of a known account wherever it stands as a text of its own, whatever
// it looks like ("ops@corp" of a provider inside a network, a top-level name in its ASCII form): the panel
// knows these and need not guess. And then what looks like an address: text around an "@" whose right side has
// a dot and ends in letters. "root@pam" and "root@192.0.2.7" in an error text are left alone (unless an
// account has that very address). The rest of the sentence is not touched.
func (p *People) ShownIn(text string) string {
	if strings.Contains(text, "@") {
		text = p.addresses(text)
	}
	for _, name := range p.names {
		text = p.words(text, name)
	}
	return text
}

// HiddenIn is a sentence this version made, as it is handed out. The people the panel wrote into it are there
// by what the panel shows them by already. Whatever else is in it came from outside (an error text of an
// agent, a name on a machine), and nothing of it is looked up: every e-mail address (ShownIn says what one is)
// becomes HiddenAddress, and the name of a known account without an address HiddenName where the sentence
// names who did something (words), whoever has them. An answer with a user name in it would tell whoever
// supplied the text whose address or name that is. A text that is what an account is shown by stays: the
// panel wrote it.
func (p *People) HiddenIn(text string) string {
	nobody := *p
	nobody.shown = nil
	return nobody.ShownIn(text)
}

func localRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._%+-'", r)
}

func domainRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-'
}

// addresses replaces the addresses in a sentence: the known ones where they stand (known), and between them
// what looks like one (byShape). What is put in is not read again.
func (p *People) addresses(text string) string {
	var b strings.Builder
	for {
		i, n := p.known(text)
		if i < 0 {
			b.WriteString(p.byShape(text))
			return b.String()
		}
		b.WriteString(p.byShape(text[:i]))
		b.WriteString(p.address(text[i : i+n]))
		text = text[i+n:]
		for _, mark := range []string{UnverifiedMark, agentMark} { // the mark was about the address
			if strings.HasPrefix(text, mark) {
				text = text[len(mark):]
				break
			}
		}
	}
}

// known finds the first place in text where the address of a known account stands as a text of its own, in
// any case of its letters: at is where it begins and n how long it is there (at is -1 for none). "Of its own":
// what stands before it is no part of an address (a letter, a digit, one of . _ % + - ' or an "@"), and
// neither is what follows, so "ops@corp" is not found in "devops@corp" or in "ops@corp.example". A full stop
// or a dash behind it is a part only when the address goes on after it: one that ends the sentence is not.
func (p *People) known(text string) (at, n int) {
	if len(p.addrs) == 0 {
		return -1, 0
	}
	for i := range text {
		if before, _ := utf8.DecodeLastRuneInString(text[:i]); i > 0 && (localRune(before) || before == '@') {
			continue
		}
		for _, addr := range p.addrs { // longest first
			n, ok := prefixFold(text[i:], addr)
			if !ok {
				continue
			}
			rest := text[i+n:]
			after, size := utf8.DecodeRuneInString(rest)
			if after == '.' || after == '-' {
				after, _ = utf8.DecodeRuneInString(rest[size:])
			}
			if rest == "" || !(unicode.IsLetter(after) || unicode.IsDigit(after) || after == '@' || after == '_') {
				return i, n
			}
		}
	}
	return -1, 0
}

// prefixFold reports whether text begins with want in any case of its letters, and how many bytes of text
// that beginning is.
func prefixFold(text, want string) (n int, ok bool) {
	for _, w := range want {
		r, size := utf8.DecodeRuneInString(text[n:])
		if size == 0 || (r != w && fold(string(r)) != fold(string(w))) {
			return 0, false
		}
		n += size
	}
	return n, true
}

// byShape replaces what looks like an address in text (isAddress).
func (p *People) byShape(text string) string {
	if !strings.Contains(text, "@") {
		return text
	}
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
	if subs, _, _ := p.match(addr); len(subs) == 1 {
		if n := p.shown[subs[0]]; n != "" {
			return n
		}
	}
	return HiddenAddress
}

// words replaces name (the name of an account without an address) in text where the sentence names who did
// something, and nowhere else. The sentences of the versions that named such an account by its name have who
// did it in two places, and the name is looked for there alone:
//
//   - after the word "by" and one space: "switched off by X", "started by X: 3 machines", "requested by X
//     (...)", "run started by X was cancelled by Y", "Keep on by X: ...", "panel by X";
//   - after "the run" and one space: "the panel restarted during the run X started at ...", "woke ... for the
//     run X started".
//
// And there only as a word of its own (what follows is no letter and no digit). Until the fix round after the
// fifth review a name was replaced wherever it stood as a word: an account that called itself "off" took the
// word out of "switched off by Juniper" for everybody.
//
// A name that is what an account is shown by right now is not replaced at all: it is the text the panel
// itself writes for that account, and a record with it is that account's (match).
func (p *People) words(text, name string) string {
	if !strings.Contains(text, name) {
		return text
	}
	if _, isShown := p.byShown[fold(name)]; isShown {
		return text
	}
	repl := HiddenName
	if subs, _, _ := p.match(name); len(subs) == 1 && p.shown[subs[0]] != "" {
		repl = p.shown[subs[0]]
	}
	edge := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	var b strings.Builder
	done := 0 // text[:done] is written
	for at := 0; ; {
		i := strings.Index(text[at:], name)
		if i < 0 {
			break
		}
		i += at
		at = i + len(name)
		after, _ := utf8.DecodeRuneInString(text[at:])
		if namesWho(text[:i]) && (at == len(text) || edge(after)) {
			b.WriteString(text[done:i])
			b.WriteString(repl)
			done = at
		}
	}
	b.WriteString(text[done:])
	return b.String()
}

// namesWho reports whether what follows before in a sentence is who did something: before ends in the word
// "by" (in any case of its letters) or in the words "the run", and one space.
func namesWho(before string) bool {
	for _, lead := range []string{"by ", "the run "} {
		if len(before) < len(lead) || !strings.EqualFold(before[len(before)-len(lead):], lead) {
			continue
		}
		r, _ := utf8.DecodeLastRuneInString(before[:len(before)-len(lead)])
		if len(before) == len(lead) || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}

// Private are the addresses and names of the known accounts, longest first: what the Users page and the audit
// log draw as dots until it is clicked. For admins only.
func (p *People) Private() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, u := range p.users {
		for _, v := range []string{u.Email, u.Name} {
			if v != "" && !seen[v] && (p.shown[u.Sub] == "" || fold(v) != fold(u.Username)) { // (a name that is the shown user name is no secret)
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

// Named are the subjects of the accounts that are shown by something that holds text (in any case of its
// letters): what a search for a person by the name the panel shows finds, also in records that only have the
// subject.
func (p *People) Named(text string) []string {
	text = fold(text)
	var subs []string
	if text == "" {
		return subs
	}
	for sub, n := range p.shown {
		if strings.Contains(fold(n), text) {
			subs = append(subs, sub)
		}
	}
	sort.Strings(subs)
	return subs
}

// outsideForm is a text as an agent repeats what it was told about who asked: an agent keeps the letters A to
// Z, the digits, spaces and . _ @ - of it, and 64 bytes (Fairy-agent, cleanWho). Two texts with the same form
// cannot be told apart in what an agent answers. Here also: letters without their case, one space for many.
func outsideForm(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune(" ._@-", r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return strings.Join(strings.Fields(out), " ")
}

// outsideNobody is what an agent says when it was told nothing about who asked.
const outsideNobody = "unknown"

// FromOutside is a text that names a person and was supplied by somebody outside the panel: what a machine's
// agent answers as "requested by" of a countdown. The agent repeats what a panel told it, and whoever runs the
// agent can make it say anything. So the text is never looked up among the addresses and names of the
// accounts: an answer "Juniper" for "owner@example.com" would tell them whose address that is.
//
//   - it is what exactly one account is shown by (as an agent repeats it: outsideForm): that;
//   - it is the panel's own words for a person it does not name, or the agent's for nobody: those words;
//   - anything else with an "@": HiddenAddress; anything else: HiddenName. The panel cannot tell a name from
//     any other text, and shows neither.
func (p *People) FromOutside(text string) string {
	k := outsideForm(text)
	switch subs := p.outside[k]; {
	case strings.TrimSpace(text) == "":
		return text
	case len(subs) == 1:
		return p.shown[subs[0]]
	case k == outsideForm(HiddenAddress):
		return HiddenAddress
	case k == outsideForm(HiddenName):
		return HiddenName
	case k == outsideNobody && len(subs) == 0:
		return outsideNobody
	case strings.Contains(text, "@"):
		return HiddenAddress
	}
	return HiddenName
}
