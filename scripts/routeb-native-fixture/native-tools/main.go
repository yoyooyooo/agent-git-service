// Command native-tools prepares only fresh fixture data and inspects real ELF
// inputs. It is not an application launcher or a native admission substitute.
package main

import (
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func inspectELF(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err = io.ReadFull(f, magic); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		return map[string]any{"native": false}, nil
	}
	if string(magic) != "\x7fELF" {
		return map[string]any{"native": false}, nil
	}
	image, err := elf.NewFile(f)
	if err != nil {
		return nil, err
	}
	if image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB || image.Machine != elf.EM_X86_64 {
		return nil, errors.New("elf_architecture")
	}
	stringsFor := func(tag elf.DynTag) ([]string, error) {
		values, err := image.DynString(tag)
		if values == nil {
			values = []string{}
		}
		return values, err
	}
	needed, err := stringsFor(elf.DT_NEEDED)
	if err != nil {
		return nil, err
	}
	rpath, err := stringsFor(elf.DT_RPATH)
	if err != nil {
		return nil, err
	}
	runpath, err := stringsFor(elf.DT_RUNPATH)
	if err != nil {
		return nil, err
	}
	interpreter := ""
	for _, program := range image.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(program.Open(), 4097))
		if err != nil || len(raw) < 2 || len(raw) > 4096 || raw[len(raw)-1] != 0 || interpreter != "" {
			return nil, errors.New("elf_interpreter")
		}
		interpreter = string(raw[:len(raw)-1])
	}
	return map[string]any{"native": true, "image": interpreterauthority.NativeImage{
		Path: path, Format: "elf", Interpreter: interpreter, Needed: needed,
		RPath: rpath, RunPath: runpath, Resolution: map[string]string{},
	}}, nil
}

func seed(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "main.db" ||
		!strings.Contains(path, "/.local/state/ags-routeb-native/") || os.Getuid() == 0 {
		return errors.New("fixture_database_path")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	database, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return err
	}
	sqlDB, err := database.DB()
	if err != nil {
		return err
	}
	migrateErr := db.Migrate(database)
	closeErr := sqlDB.Close()
	return errors.Join(migrateErr, closeErr)
}

func run(args []string) (any, error) {
	if len(args) == 2 && args[0] == "inspect-elf" {
		return inspectELF(args[1])
	}
	if len(args) == 2 && args[0] == "seed-db" {
		if err := seed(args[1]); err != nil {
			return nil, err
		}
		return map[string]any{"schema": "ags.routeb-fixture-seed.v1", "status": "migrated", "native_pass": false}, nil
	}
	if len(args) == 2 && args[0] == "verify-catalog" {
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return nil, err
		}
		var input struct {
			ID      string                       `json:"id"`
			SHA256  string                       `json:"sha256"`
			Binding interpreterauthority.Binding `json:"binding"`
		}
		if err = interpreterauthority.StrictJSON(raw, &input); err != nil {
			return nil, err
		}
		return interpreterauthority.VerifyRegistered(input.ID, input.SHA256, input.Binding)
	}
	return nil, errors.New("fixture_command")
}

func main() {
	value, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture_native_tools_failed")
		os.Exit(78)
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		os.Exit(78)
	}
}
