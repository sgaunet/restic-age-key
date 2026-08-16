package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"restic-age-key": main,
	})
}

func TestScript(t *testing.T) {
	updateScripts, _ := strconv.ParseBool(os.Getenv("UPDATE_SCRIPTS"))

	testscript.Run(t, testscript.Params{
		Dir:             "testdata",
		ContinueOnError: true,
		UpdateScripts:   updateScripts,
		Setup:           setupEnv,
	})
}

// setupEnv restores $HOME inside the script environment. testscript pins it to
// /no-home, which breaks version-manager shims (mise, asdf): they resolve the
// tool they stand for relative to $HOME, so `jq` — used by the scripts that
// inspect `restic key list --json` — fails on machines that install it that
// way. restic's cache is redirected into $WORK so the real one is neither read
// nor written.
func setupEnv(env *testscript.Env) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	env.Setenv("HOME", home)
	env.Setenv("RESTIC_CACHE_DIR", filepath.Join(env.WorkDir, "restic-cache"))

	return nil
}
