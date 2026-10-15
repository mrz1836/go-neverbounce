package neverbounce

import "slices"

// Result is the provider's verdict on one address. The set is open: a value
// the provider adds later is kept as sent when it has the documented token
// shape (lowercase letters, digits and "_", up to 64 bytes), and Known reports
// false for it; any other value fails the call as unexpected_response.
// There is deliberately no "safe to send" helper: what to do with each
// verdict is the caller's policy.
type Result string

// Documented results.
const (
	// ResultValid means the provider verified the address as real.
	ResultValid Result = "valid"

	// ResultInvalid means the provider verified the address as not valid.
	ResultInvalid Result = "invalid"

	// ResultDisposable means a temporary, disposable address.
	ResultDisposable Result = "disposable"

	// ResultCatchall means the domain accepts all mail (accept-all), so the
	// address itself could not be verified. It is not a "safe" verdict.
	ResultCatchall Result = "catchall"

	// ResultUnknown means the provider could not reach the server. It is a
	// successful answer, never an error, and the check was still billed.
	ResultUnknown Result = "unknown"
)

// Known reports whether r is one of the five documented results.
func (r Result) Known() bool {
	switch r {
	case ResultValid, ResultInvalid, ResultDisposable, ResultCatchall, ResultUnknown:
		return true
	}
	return false
}

// Flag is one provider observation about an address or its domain. The set is
// open: a flag the provider adds later is kept as sent when it has the token
// shape Result describes, and Known reports false for it. v4.2 spells academic_host; the v4.1 spelling "acedemic_host" is kept
// verbatim if it ever appears, never normalized.
type Flag string

// Documented v4.2 flags, in the provider's order.
const (
	FlagHasDNS             Flag = "has_dns"
	FlagHasDNSMX           Flag = "has_dns_mx"
	FlagBadSyntax          Flag = "bad_syntax"
	FlagFreeEmailHost      Flag = "free_email_host"
	FlagProfanity          Flag = "profanity"
	FlagRoleAccount        Flag = "role_account"
	FlagDisposableEmail    Flag = "disposable_email"
	FlagGovernmentHost     Flag = "government_host"
	FlagAcademicHost       Flag = "academic_host"
	FlagMilitaryHost       Flag = "military_host"
	FlagInternationalHost  Flag = "international_host"
	FlagSquatterHost       Flag = "squatter_host"
	FlagSpellingMistake    Flag = "spelling_mistake"
	FlagBadDNS             Flag = "bad_dns"
	FlagTemporaryDNSError  Flag = "temporary_dns_error"
	FlagConnectFails       Flag = "connect_fails"
	FlagAcceptsAll         Flag = "accepts_all"
	FlagContainsAlias      Flag = "contains_alias"
	FlagContainsSubdomain  Flag = "contains_subdomain"
	FlagSMTPConnectable    Flag = "smtp_connectable"
	FlagSpamtrapNetwork    Flag = "spamtrap_network"
	FlagHistoricalResponse Flag = "historical_response"
)

// Known reports whether f is one of the 22 documented v4.2 flags.
func (f Flag) Known() bool {
	switch f {
	case FlagHasDNS, FlagHasDNSMX, FlagBadSyntax, FlagFreeEmailHost, FlagProfanity, FlagRoleAccount,
		FlagDisposableEmail, FlagGovernmentHost, FlagAcademicHost, FlagMilitaryHost, FlagInternationalHost,
		FlagSquatterHost, FlagSpellingMistake, FlagBadDNS, FlagTemporaryDNSError, FlagConnectFails,
		FlagAcceptsAll, FlagContainsAlias, FlagContainsSubdomain, FlagSMTPConnectable, FlagSpamtrapNetwork,
		FlagHistoricalResponse:
		return true
	}
	return false
}

// Flags is the provider's flag list, in the provider's order, with duplicates
// kept. An empty list is nil.
type Flags []Flag

// Has reports whether fs contains f, compared exactly.
func (fs Flags) Has(f Flag) bool { return slices.Contains(fs, f) }

// AddressInfo is the provider's parse of the address ("address_info"). Every
// field is provider text, never checked or normalized by the SDK; "" means
// absent, null or empty.
type AddressInfo struct {
	OriginalEmail   string `json:"original_email,omitempty"`
	NormalizedEmail string `json:"normalized_email,omitempty"`
	Addr            string `json:"addr,omitempty"`
	Alias           string `json:"alias,omitempty"`
	Host            string `json:"host,omitempty"`
	FQDN            string `json:"fqdn,omitempty"`
	Domain          string `json:"domain,omitempty"`
	Subdomain       string `json:"subdomain,omitempty"`
	TLD             string `json:"tld,omitempty"`
}

// CreditsInfo is the account's credit counters ("credits_info"), as the
// provider reported them with this answer. A nil field was not reported; a
// pointer to 0 was reported as zero. The SDK does not interpret them.
type CreditsInfo struct {
	PaidCreditsUsed      *int64 `json:"paid_credits_used,omitempty"`
	FreeCreditsUsed      *int64 `json:"free_credits_used,omitempty"`
	PaidCreditsRemaining *int64 `json:"paid_credits_remaining,omitempty"`
	FreeCreditsRemaining *int64 `json:"free_credits_remaining,omitempty"`
}

// verificationNames are the verification members shared by single/check and
// each jobs/results row, by index from verResult on.
const (
	verResult = iota
	verFlags
	verSuggestedCorrection
	verAddressInfo
	verCount
)

// verificationNames returns the provider names of the verification members,
// by index.
func verificationNames() [verCount]string {
	return [verCount]string{
		verResult:              "result",
		verFlags:               "flags",
		verSuggestedCorrection: "suggested_correction",
		verAddressInfo:         "address_info",
	}
}

// verification is the decoded shared part of a verdict.
type verification struct {
	result              Result
	flags               Flags
	suggestedCorrection string
	addressInfo         *AddressInfo
}

// decodeVerification decodes the shared verdict members, given their raw
// values in verification-member order, at path p.
func decodeVerification(raw [verCount][]byte, p path) (verification, *decodeError) {
	var v verification
	result, derr := decodeToken(raw[verResult], "", true)
	if derr != nil {
		derr.field = p.member("result")
		return v, derr
	}
	v.result = Result(result)
	if v.flags, derr = decodeFlags(raw[verFlags], p); derr != nil {
		return v, derr
	}
	if v.suggestedCorrection, derr = decodeText(raw[verSuggestedCorrection], ""); derr != nil {
		derr.field = p.member("suggested_correction")
		return v, derr
	}
	if v.addressInfo, derr = decodeAddressInfo(raw[verAddressInfo], p); derr != nil {
		return v, derr
	}
	return v, nil
}

// decodeAddressInfo decodes the optional address_info member of the verdict
// at p.
func decodeAddressInfo(raw []byte, p path) (*AddressInfo, *decodeError) {
	switch {
	case isNull(raw):
		return nil, nil
	case raw[0] != '{':
		return nil, wrongType(p.member("address_info"))
	}
	a := &AddressInfo{}
	slots := [...]slot[string]{
		{"original_email", &a.OriginalEmail},
		{"normalized_email", &a.NormalizedEmail},
		{"addr", &a.Addr},
		{"alias", &a.Alias},
		{"host", &a.Host},
		{"fqdn", &a.FQDN},
		{"domain", &a.Domain},
		{"subdomain", &a.Subdomain},
		{"tld", &a.TLD},
	}
	if i, derr := decodeSlots(raw, slots[:], decodeText); derr != nil {
		derr.field = p.member("address_info") + "." + slots[i].name
		return nil, derr
	}
	return a, nil
}

// decodeCreditsInfo decodes an optional credits_info object.
func decodeCreditsInfo(raw []byte) (*CreditsInfo, *decodeError) {
	const field = "credits_info"
	switch {
	case isNull(raw):
		return nil, nil
	case raw[0] != '{':
		return nil, wrongType(field)
	}
	ci := &CreditsInfo{}
	slots := [...]slot[*int64]{
		{"paid_credits_used", &ci.PaidCreditsUsed},
		{"free_credits_used", &ci.FreeCreditsUsed},
		{"paid_credits_remaining", &ci.PaidCreditsRemaining},
		{"free_credits_remaining", &ci.FreeCreditsRemaining},
	}
	if i, derr := decodeSlots(raw, slots[:], decodeCount); derr != nil {
		derr.field = field + "." + slots[i].name
		return nil, derr
	}
	return ci, nil
}
