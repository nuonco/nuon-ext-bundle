package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func ctxCmd() *cobra.Command {
	var current, unset, del, save bool
	c := &cobra.Command{
		Use:   "ctx [<name> | - | <new>=<old>]",
		Short: "manage named deployment contexts",
		Long: `Manage named deployment contexts.

  nuon-bundle ctx                 list contexts
  nuon-bundle ctx <name>          switch to a context
  nuon-bundle ctx -               switch to the previous context
  nuon-bundle ctx -c              print the active context name
  nuon-bundle ctx <new>=<old>     rename a context (<old> may be . for the active one)
  nuon-bundle ctx -s <name>       save the current config as a named context and activate it
  nuon-bundle ctx -s <name> <file> copy a config file into a named context
  nuon-bundle ctx -d <name>...    delete contexts (. deletes the active one)
  nuon-bundle ctx -u              unlink the active context`,
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := defaultConfigPaths()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch {
			case current:
				name, err := currentContext(paths)
				if err != nil {
					return err
				}
				fmt.Fprintln(w, name)
				return nil
			case unset:
				return unsetContext(paths)
			case del:
				if len(args) == 0 {
					return fmt.Errorf("-d requires at least one context name")
				}
				return deleteContexts(paths, args)
			case save:
				switch len(args) {
				case 1:
					return saveContext(paths, args[0])
				case 2:
					return copyContext(paths, args[0], args[1])
				default:
					return fmt.Errorf("-s requires a context name and optionally a source file")
				}
			}
			switch len(args) {
			case 0:
				return listContexts(w, paths)
			case 1:
				if args[0] == "-" {
					return switchPrevious(paths)
				}
				if newName, oldName, ok := strings.Cut(args[0], "="); ok {
					return renameContext(paths, newName, oldName)
				}
				return switchContext(paths, args[0])
			default:
				return fmt.Errorf("too many arguments")
			}
		},
	}
	c.Flags().BoolVarP(&current, "current", "c", false, "print the active context name")
	c.Flags().BoolVarP(&unset, "unset", "u", false, "unlink the active context")
	c.Flags().BoolVarP(&del, "delete", "d", false, "delete the named contexts")
	c.Flags().BoolVarP(&save, "save", "s", false, "save the current config as a named context")
	return c
}

func contextPath(paths configPaths, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid context name %q", name)
	}
	return filepath.Join(paths.contexts, name), nil
}

func currentContext(paths configPaths) (string, error) {
	info, err := os.Lstat(paths.active)
	if err != nil {
		return "", fmt.Errorf("no active context: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s is not a context symlink; save it with `nuon-bundle ctx -s <name>`", paths.active)
	}
	target, err := os.Readlink(paths.active)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

func listContexts(w io.Writer, paths configPaths) error {
	entries, err := os.ReadDir(paths.contexts)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	active, _ := currentContext(paths)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		fmt.Fprintln(w, "No contexts found. Use `nuon-bundle ctx -s <name>` to save the current config as a context.")
		return nil
	}
	for _, name := range names {
		marker := "  "
		if name == active {
			marker = "* "
		}
		fmt.Fprintln(w, marker+name)
	}
	return nil
}

func switchContext(paths configPaths, name string) error {
	target, err := contextPath(paths, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("context %q not found", name)
	}
	if info, err := os.Lstat(paths.active); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is a regular file; save it first with `nuon-bundle ctx -s <name>`", paths.active)
		}
		if previous, err := currentContext(paths); err == nil {
			_ = os.MkdirAll(paths.contexts, 0o755)
			_ = os.WriteFile(filepath.Join(paths.contexts, ".previous"), []byte(previous), 0o600)
		}
		if err := os.Remove(paths.active); err != nil {
			return err
		}
	}
	return os.Symlink(target, paths.active)
}

func switchPrevious(paths configPaths) error {
	raw, err := os.ReadFile(filepath.Join(paths.contexts, ".previous"))
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return fmt.Errorf("no previous context found")
	}
	return switchContext(paths, strings.TrimSpace(string(raw)))
}

func saveContext(paths configPaths, name string) error {
	target, err := contextPath(paths, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("context %q already exists", name)
	}
	if err := os.MkdirAll(paths.contexts, 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(paths.active)
	if os.IsNotExist(err) {
		raw = nil
	} else if err != nil {
		return fmt.Errorf("read %s: %w", paths.active, err)
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(paths.active); err != nil {
		return err
	}
	return os.Symlink(target, paths.active)
}

func copyContext(paths configPaths, name, source string) error {
	target, err := contextPath(paths, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("context %q already exists", name)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.contexts, 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, raw, 0o600)
}

func deleteContexts(paths configPaths, names []string) error {
	for _, name := range names {
		if name == "." {
			resolved, err := currentContext(paths)
			if err != nil {
				return err
			}
			name = resolved
		}
		target, err := contextPath(paths, name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(target); err != nil {
			return fmt.Errorf("context %q not found", name)
		}
		if active, err := currentContext(paths); err == nil && active == name {
			if err := os.Remove(paths.active); err != nil {
				return err
			}
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	return nil
}

func renameContext(paths configPaths, newName, oldName string) error {
	if oldName == "." {
		resolved, err := currentContext(paths)
		if err != nil {
			return err
		}
		oldName = resolved
	}
	oldPath, err := contextPath(paths, oldName)
	if err != nil {
		return err
	}
	newPath, err := contextPath(paths, newName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("context %q not found", oldName)
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("context %q already exists", newName)
	}
	active, activeErr := currentContext(paths)
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	if activeErr == nil && active == oldName {
		if err := os.Remove(paths.active); err != nil {
			return err
		}
		return os.Symlink(newPath, paths.active)
	}
	return nil
}

func unsetContext(paths configPaths) error {
	info, err := os.Lstat(paths.active)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s is not a symlink, refusing to remove", paths.active)
	}
	return os.Remove(paths.active)
}
