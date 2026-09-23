package replicationadmin

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

const usage = `ags-replication: explicit operator enrollment (no automatic rollout)
  status    --primary URL --repo OWNER/REPO --token-file FILE [--ca-file FILE] [--allow-private-http]
  register  --primary URL --expected STATUS_JSON --token-file FILE [--ca-file FILE] [--allow-private-http]
  peer-plan --edge-id ID --certificate FILE --ca-file FILE --registration REGISTERED_JSON [--registration ...]

JSON results go to stdout. Token files must be regular owner-only files.
status is read-only. register sends one row-bound POST; after an uncertain result,
inspect status instead of automatically retrying. peer-plan verifies an existing
client certificate and emits unapplied primary-peer/Edge-binding fragments.
This command never creates keys, edits configuration, restarts services, opens a
business database, or changes any Git remote, DNS, or Tailscale setting.
`

type stringList []string

func (s *stringList) String() string         { return "" }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Never echo possibly sensitive argv into errors.
	var result any
	switch args[0] {
	case "status", "register":
		primary := fs.String("primary", "", "primary origin")
		tokenFile := fs.String("token-file", "", "private credential file")
		caFile := fs.String("ca-file", "", "optional HTTPS trust bundle")
		allow := fs.Bool("allow-private-http", false, "explicit secured-network HTTP opt-in")
		timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
		repository, expectedPath := "", ""
		if args[0] == "status" {
			fs.StringVar(&repository, "repo", "", "repository")
		} else {
			fs.StringVar(&expectedPath, "expected", "", "prior status JSON")
		}
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("invalid operator arguments; use help (credentials must be in a file)")
		}
		token, err := readFile(*tokenFile, true, 16<<10)
		if err != nil {
			return errors.New("token file must be a bounded owner-only regular file")
		}
		credential := strings.TrimSpace(string(token))
		cfg := ClientConfig{PrimaryURL: *primary, AllowPrivateHTTP: *allow, Timeout: *timeout}
		if *caFile != "" {
			data, err := readFile(*caFile, false, 1<<20)
			if err != nil {
				return errors.New("cannot read primary CA")
			}
			certs, err := certificates(data)
			if err != nil {
				return err
			}
			cfg.RootCAs = x509.NewCertPool()
			for _, cert := range certs {
				if !cert.IsCA {
					return errors.New("primary CA bundle contains a leaf certificate")
				}
				cfg.RootCAs.AddCert(cert)
			}
		}
		client, err := NewClient(cfg)
		if err != nil {
			return err
		}
		defer client.Close()
		if args[0] == "status" {
			result, err = client.Status(ctx, repository, credential)
		} else {
			expected, decodeErr := readRegistration(expectedPath)
			if decodeErr != nil {
				return decodeErr
			}
			result, err = client.Register(ctx, expected, credential)
		}
		if err != nil {
			return err
		}
	case "peer-plan":
		edgeID := fs.String("edge-id", "", "node identifier")
		certificate := fs.String("certificate", "", "node client certificate")
		caFile := fs.String("ca-file", "", "peer CA bundle")
		var paths stringList
		fs.Var(&paths, "registration", "registered repository JSON")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || len(paths) == 0 || len(paths) > 256 {
			return errors.New("invalid peer-plan arguments; use help")
		}
		cert, err := readFile(*certificate, false, 1<<20)
		if err != nil {
			return errors.New("cannot read node certificate")
		}
		ca, err := readFile(*caFile, false, 1<<20)
		if err != nil {
			return errors.New("cannot read peer CA")
		}
		var registrations []edgeprotocol.Registration
		for _, path := range paths {
			registered, err := readRegistration(path)
			if err != nil {
				return err
			}
			registrations = append(registrations, registered)
		}
		result, err = BuildPeerPlan(*edgeID, cert, ca, registrations, time.Now().UTC())
		if err != nil {
			return err
		}
	default:
		return errors.New("unknown operator command; use help")
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errors.New("cannot encode operator result")
	}
	if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
		return errors.New("operator output failed; inspect status before retrying a mutation")
	}
	return nil
}

func readRegistration(path string) (edgeprotocol.Registration, error) {
	data, err := readFile(path, false, edgeprotocol.MaxControlBytes)
	if err != nil {
		return edgeprotocol.Registration{}, errors.New("cannot read registration receipt")
	}
	var result edgeprotocol.Registration
	if edgeprotocol.DecodeControl(bytes.NewReader(data), &result) != nil || result.Validate() != nil {
		return edgeprotocol.Registration{}, errors.New("invalid closed registration receipt")
	}
	return result, nil
}

func readFile(path string, private bool, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit || private && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid operator file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("operator file unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("operator file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("operator file read failed")
	}
	return data, nil
}
