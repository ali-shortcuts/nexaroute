package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Regression test for a real defect found by the 2026-10-15 audit: the
// `config` subcommand existed and was unit-tested through direct
// runConfig calls, but main() never dispatched it, so
// `nexaroute config validate` silently started the gateway (proven
// against a binary built from main). The routing now lives in
// dispatchSubcommand and this test pins every subcommand's routing —
// with the config case REMOVED, the config subtest fails with
// handled=false instead of validating.
func TestDispatchSubcommandRouting(t *testing.T) {
	configPath := writeTestConfig(t, config.Default())

	t.Run("config validate reaches runConfig", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, handled := dispatchSubcommand([]string{"config", "validate", "--config", configPath}, &stdout, &stderr)
		if !handled {
			t.Fatal("config subcommand is not dispatched; the binary would start the gateway instead")
		}
		if code != 0 || !strings.Contains(stdout.String(), "config valid") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("every documented subcommand is handled", func(t *testing.T) {
		for _, args := range [][]string{
			{"privacy"},                        // usage -> nonzero, but handled
			{"audit"},                          // usage -> nonzero, but handled
			{"config"},                         // usage -> nonzero, but handled
			{"doctor", "--config", configPath}, // healthy fixture -> 0
			{"backup"},                         // missing --out -> 2, but handled
			{"restore"},                        // missing --bundle -> 2, but handled
			{"secrets", "definitely-not-a-subcommand"}, // error -> 1, but handled
		} {
			var stdout, stderr bytes.Buffer
			if _, handled := dispatchSubcommand(args, &stdout, &stderr); !handled {
				t.Fatalf("%v is not dispatched", args)
			}
		}
	})

	t.Run("gateway invocations are not handled", func(t *testing.T) {
		for _, args := range [][]string{
			{},
			{"--config", "x.json"},
			{"--version"},
			{"--no-browser"},
		} {
			var stdout, stderr bytes.Buffer
			if _, handled := dispatchSubcommand(args, &stdout, &stderr); handled {
				t.Fatalf("%v must fall through to gateway startup", args)
			}
		}
	})
}
