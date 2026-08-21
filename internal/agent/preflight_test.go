package agent

import "testing"

// Real `docker compose config` output, captured from Compose v2. The warning line is the one that
// matters: it is what a credential supplied by a deploy wrapper looks like when anything else
// recreates the container.
const composeWarn = `time="2026-08-21T03:32:24+02:00" level=warning msg="The \"IONOS_API_PREFIX\" variable is not set. Defaulting to a blank string."
time="2026-08-21T03:32:24+02:00" level=warning msg="The \"IONOS_API_SECRET\" variable is not set. Defaulting to a blank string."`

const composeErr = `time="2026-08-21T03:32:24+02:00" level=warning msg="The \"MAY_BE_BLANK\" variable is not set. Defaulting to a blank string."
error while interpolating services.guarded.environment.[]: required variable MUST_EXIST is missing a value: missing on purpose`

func TestBlankVars_NamesEveryVariableComposeWouldBlank(t *testing.T) {
	got := blankVars(composeWarn)
	if len(got) != 2 || got[0] != "IONOS_API_PREFIX" || got[1] != "IONOS_API_SECRET" {
		t.Fatalf("got %v, want both IONOS variables", got)
	}
	// deduplicated: compose warns once per reference, not once per variable
	if n := len(blankVars(composeWarn + "\n" + composeWarn)); n != 2 {
		t.Errorf("repeated warnings should collapse, got %d names", n)
	}
}

func TestBlankVars_QuietOnAHealthyProject(t *testing.T) {
	if got := blankVars(""); len(got) != 0 {
		t.Errorf("a clean render must report nothing, got %v", got)
	}
}

func TestFirstInterpolationError(t *testing.T) {
	if got := firstInterpolationError(composeErr); got == "" {
		t.Fatal("a guarded-variable failure should be surfaced")
	}
	if got := firstInterpolationError(composeWarn); got != "" {
		t.Errorf("warnings are not interpolation errors, got %q", got)
	}
}
