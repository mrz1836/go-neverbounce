package neverbounce_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
)

// pvDocumentedResults returns the five documented results, spelled out
// independently of the library.
func pvDocumentedResults() []neverbounce.Result {
	return []neverbounce.Result{"valid", "invalid", "disposable", "catchall", "unknown"}
}

// pvDocumentedFlags returns the 22 documented v4.2 flags in the provider's
// order, spelled out independently of the library.
func pvDocumentedFlags() []neverbounce.Flag {
	return []neverbounce.Flag{
		"has_dns", "has_dns_mx", "bad_syntax", "free_email_host", "profanity", "role_account",
		"disposable_email", "government_host", "academic_host", "military_host", "international_host",
		"squatter_host", "spelling_mistake", "bad_dns", "temporary_dns_error", "connect_fails", "accepts_all",
		"contains_alias", "contains_subdomain", "smtp_connectable", "spamtrap_network", "historical_response",
	}
}

// pvUnknownTokens are values outside every documented vocabulary.
func pvUnknownTokens() []string {
	return []string{"", " ", "VALID", "Valid", "valid ", "acedemic_host", "has-dns", "complete\n", "über", "x"}
}

func TestVerdictResultKnown(t *testing.T) {
	results := pvDocumentedResults()
	assert.Equal(t, []neverbounce.Result{
		neverbounce.ResultValid, neverbounce.ResultInvalid, neverbounce.ResultDisposable,
		neverbounce.ResultCatchall, neverbounce.ResultUnknown,
	}, results, "the constants spell the documented values")
	for _, r := range results {
		assert.True(t, r.Known(), r)
	}
	for _, s := range append(pvUnknownTokens(), "risky", "accept_all", "deliverable") {
		assert.False(t, neverbounce.Result(s).Known(), "%q", s)
	}
}

func TestVerdictFlagKnown(t *testing.T) {
	flags := pvDocumentedFlags()
	require.Len(t, flags, 22)
	assert.Equal(t, []neverbounce.Flag{
		neverbounce.FlagHasDNS, neverbounce.FlagHasDNSMX, neverbounce.FlagBadSyntax, neverbounce.FlagFreeEmailHost,
		neverbounce.FlagProfanity, neverbounce.FlagRoleAccount, neverbounce.FlagDisposableEmail,
		neverbounce.FlagGovernmentHost, neverbounce.FlagAcademicHost, neverbounce.FlagMilitaryHost,
		neverbounce.FlagInternationalHost, neverbounce.FlagSquatterHost, neverbounce.FlagSpellingMistake,
		neverbounce.FlagBadDNS, neverbounce.FlagTemporaryDNSError, neverbounce.FlagConnectFails,
		neverbounce.FlagAcceptsAll, neverbounce.FlagContainsAlias, neverbounce.FlagContainsSubdomain,
		neverbounce.FlagSMTPConnectable, neverbounce.FlagSpamtrapNetwork, neverbounce.FlagHistoricalResponse,
	}, flags, "the constants spell the documented values")
	seen := map[neverbounce.Flag]bool{}
	for _, f := range flags {
		assert.True(t, f.Known(), f)
		assert.False(t, seen[f], "duplicate flag %s", f)
		seen[f] = true
	}
	for _, s := range append(pvUnknownTokens(), "has_mx", "free_email", "catchall") {
		assert.False(t, neverbounce.Flag(s).Known(), "%q", s)
	}
	assert.False(t, neverbounce.Flag("acedemic_host").Known(), "the v4.1 spelling is not a v4.2 flag")
}

func TestVerdictAPIStatusKnown(t *testing.T) {
	documented := []neverbounce.APIStatus{
		"success", "general_failure", pvStatusAuth, pvStatusUnavail, "throttle_triggered", "bad_referrer",
	}
	assert.Equal(t, []neverbounce.APIStatus{
		neverbounce.APIStatusSuccess, neverbounce.APIStatusGeneralFailure, neverbounce.APIStatusAuthFailure,
		neverbounce.APIStatusTempUnavail, neverbounce.APIStatusThrottleTriggered, neverbounce.APIStatusBadReferrer,
	}, documented)
	for _, s := range documented {
		assert.True(t, s.Known(), s)
	}
	for _, s := range append(pvUnknownTokens(), "SUCCESS", "Success", "failure", "maintenance_mode", "error") {
		assert.False(t, neverbounce.APIStatus(s).Known(), "%q", s)
	}
}

func TestVerdictFlagsHas(t *testing.T) {
	var none neverbounce.Flags
	assert.False(t, none.Has(neverbounce.FlagHasDNS), "a nil list has nothing")
	assert.False(t, none.Has(""), "a nil list has not even the empty flag")
	assert.False(t, neverbounce.Flags{}.Has(neverbounce.FlagHasDNS))

	fs := neverbounce.Flags{neverbounce.FlagHasDNS, "acedemic_host", neverbounce.FlagHasDNS, neverbounce.FlagRoleAccount}
	assert.True(t, fs.Has(neverbounce.FlagHasDNS), "a duplicated flag is found")
	assert.True(t, fs.Has(neverbounce.FlagRoleAccount))
	assert.True(t, fs.Has("acedemic_host"), "unknown flags are kept and found")
	assert.False(t, fs.Has(neverbounce.FlagAcademicHost), "never normalized to the v4.2 spelling")
	assert.False(t, fs.Has("HAS_DNS"), "compared exactly")
	assert.False(t, fs.Has("has_dns "), "compared exactly")
	assert.False(t, fs.Has(""))
	assert.Len(t, fs, 4, "Has never changes the list")
}

func TestVerdictJobStatePredicates(t *testing.T) {
	type class int
	const (
		inProgress class = iota
		needsAction
		terminal
	)
	states := []struct {
		state neverbounce.JobState
		value string
		class class
	}{
		{neverbounce.JobStateUploading, "uploading", inProgress},
		{neverbounce.JobStateParsing, "parsing", inProgress},
		{neverbounce.JobStateWaiting, "waiting", needsAction},
		{neverbounce.JobStateWaitingAnalyzed, "waiting_analyzed", needsAction},
		{neverbounce.JobStateQueued, "queued", inProgress},
		{neverbounce.JobStateRunning, "running", inProgress},
		{neverbounce.JobStateUnderReview, "under_review", inProgress},
		{neverbounce.JobStateComplete, "complete", terminal},
		{neverbounce.JobStateFailed, "failed", terminal},
	}
	for _, tt := range states {
		t.Run(tt.value, func(t *testing.T) {
			s := tt.state
			assert.Equal(t, neverbounce.JobState(tt.value), s)
			assert.True(t, s.Known())
			assert.Equal(t, tt.class == inProgress, s.InProgress(), "InProgress")
			assert.Equal(t, tt.class == needsAction, s.NeedsAction(), "NeedsAction")
			assert.Equal(t, tt.class == terminal, s.Terminal(), "Terminal")
		})
	}
	for _, v := range append(pvUnknownTokens(), "Complete", "archived", "paused", "waiting_review") {
		s := neverbounce.JobState(v)
		assert.False(t, s.Known(), "%q", v)
		assert.False(t, s.InProgress(), "%q", v)
		assert.False(t, s.NeedsAction(), "%q", v)
		assert.False(t, s.Terminal(), "%q", v)
	}
}

func TestVerdictJobFailureReasonKnown(t *testing.T) {
	documented := []neverbounce.JobFailureReason{"unknown_file_encoding", "empty_file", "file_too_large", "file_corrupt"}
	assert.Equal(t, []neverbounce.JobFailureReason{
		neverbounce.JobFailureUnknownFileEncoding, neverbounce.JobFailureEmptyFile,
		neverbounce.JobFailureFileTooLarge, neverbounce.JobFailureFileCorrupt,
	}, documented)
	for _, r := range documented {
		assert.True(t, r.Known(), r)
	}
	for _, v := range append(pvUnknownTokens(), "unknown", "file_missing", "EMPTY_FILE") {
		assert.False(t, neverbounce.JobFailureReason(v).Known(), "%q", v)
	}
}

func TestVerdictJobIDString(t *testing.T) {
	tests := []struct {
		id   neverbounce.JobID
		want string
	}{
		{0, "0"},
		{1, "1"},
		{289022, "289022"},
		{-1, "-1"},
		{math.MaxInt64, "9223372036854775807"},
		{math.MinInt64, "-9223372036854775808"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.id.String())
		assert.Equal(t, tt.want, fmt.Sprint(tt.id), "fmt uses String")
		assert.Equal(t, tt.want, fmt.Sprintf("%d", tt.id))
	}
}

func TestVerdictJobResultsPageMore(t *testing.T) {
	var none *neverbounce.JobResultsPage
	assert.False(t, none.More(), "a nil page has no successor")
	tests := []struct {
		page  int
		total int64
		want  bool
	}{
		{1, 2, true},
		{1, 1, false},
		{2, 1, false},
		{0, 0, false},
		{0, 1, true},
		{1, 0, false},
		{99, 100, true},
		{math.MaxInt32, math.MaxInt32, false},
		{math.MaxInt32, math.MaxInt64, true},
	}
	for _, tt := range tests {
		p := &neverbounce.JobResultsPage{Page: tt.page, TotalPages: tt.total}
		assert.Equal(t, tt.want, p.More(), "page %d of %d", tt.page, tt.total)
	}
}
