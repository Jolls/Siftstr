// Command seed loads sample content into a Siftstr instance for trying the UI,
// or exports an instance's summarized items to a seed file. It is a
// development tool and is not part of the released image.
//
//	go run ./cmd/seed load   -data ./data -user alice -file cmd/seed/example.json
//	go run ./cmd/seed load   ... -hold 1   (also sets a short undo hold)
//	go run ./cmd/seed export -data ./data -user alice > .agent-memory/my-seed.json
//
// Export copies real titles, URLs and summaries. Keep that file out of the
// repository (.agent-memory/ is ignored).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "load" && args[0] != "export") {
		return fmt.Errorf("usage: seed load|export -data DIR -user NAME [-file FILE]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := fs.String("data", "", "Siftstr data directory (holds siftstr.db)")
	user := fs.String("user", "", "username to load into or export from")
	file := fs.String("file", "", "seed file to load")
	hold := fs.Int("hold", -1, "load only: set the user's undo_hold_minutes (0 releases actions on the next minute tick)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *data == "" || *user == "" {
		return fmt.Errorf("-data and -user are required")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *data)
	if err != nil {
		return err
	}
	defer st.Close()

	if args[0] == "export" {
		f, err := export(ctx, st.DB(), *user, time.Now())
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(f)
	}
	if *file == "" {
		return fmt.Errorf("-file is required for load")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	s, i, err := load(ctx, st.DB(), *user, f, time.Now())
	if err != nil {
		return err
	}
	if *hold >= 0 {
		uid, err := userID(ctx, st.DB(), *user)
		if err != nil {
			return err
		}
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES (?, 'undo_hold_minutes', ?)
			ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, uid, strconv.Itoa(*hold)); err != nil {
			return err
		}
		fmt.Printf("undo_hold_minutes set to %d for %s\n", *hold, *user)
	}
	fmt.Printf("added %d sources and %d items for %s\n", s, i, *user)
	return nil
}
