// Command epar-prebuilt-publisher plans/promotes one immutable prebuilt
// publication and writes the append-only catalog. It intentionally performs
// no registry pushes: Buildx, attestations, catalog OCI publication, and alias
// movement remain explicit protected workflow steps.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/solutionforest/ephemeral-action-runner/internal/prebuilt"
)

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: epar-prebuilt-publisher <plan|promote|catalog|reconcile-alias|verify-catalog|verify-package> [flags]"))
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = planCommand(os.Args[2:])
	case "promote":
		err = promoteCommand(os.Args[2:])
	case "catalog":
		err = catalogCommand(os.Args[2:])
	case "reconcile-alias":
		err = reconcileAliasCommand(os.Args[2:])
	case "verify-catalog", "resolve-catalog":
		err = verifyCatalogCommand(os.Args[1], os.Args[2:])
	case "verify-package":
		err = verifyPackageCommand(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatal(err)
	}
}

func reconcileAliasCommand(args []string) error {
	flags := flag.NewFlagSet("reconcile-alias", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	catalogPath := flags.String("catalog", "", "verified signed catalog JSON path")
	profile := flags.String("profile", prebuilt.ProfileAct, "profile alias to reconcile")
	observedDigest := flags.String("observed-digest", "", "currently resolved alias digest; empty when absent")
	outputPath := flags.String("output", "", "alias reconciliation plan JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *catalogPath == "" || *outputPath == "" {
		return errors.New("reconcile-alias requires --catalog and --output")
	}
	catalog, err := readCatalog(*catalogPath)
	if err != nil {
		return err
	}
	plan, err := catalog.PlanAliasReconciliation(*profile, *observedDigest)
	if err != nil {
		return fmt.Errorf("plan alias reconciliation: %w", err)
	}
	if err := writeJSON(*outputPath, plan); err != nil {
		return fmt.Errorf("write alias reconciliation plan: %w", err)
	}
	fmt.Printf("needsRepair=%t\ntargetDigest=%s\n", plan.NeedsRepair, plan.TargetDigest)
	return nil
}

func planCommand(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	catalogPath := flags.String("catalog", "", "append-only catalog JSON path")
	inputPath := flags.String("input", "", "publication input JSON path")
	outputPath := flags.String("output", "", "publication plan JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *catalogPath == "" || *inputPath == "" || *outputPath == "" {
		return errors.New("plan requires --catalog, --input, and --output")
	}
	catalog, err := readCatalog(*catalogPath)
	if err != nil {
		return err
	}
	var input prebuilt.PublicationInput
	if err := readJSON(*inputPath, &input); err != nil {
		return fmt.Errorf("read publication input: %w", err)
	}
	resolver := prebuilt.RemoteDescriptorResolver{Authenticator: authn.Anonymous}
	plan, err := (prebuilt.Publisher{Resolver: resolver}).Plan(context.Background(), catalog, input)
	if err != nil {
		return fmt.Errorf("plan publication: %w", err)
	}
	if err := writeJSON(*outputPath, plan); err != nil {
		return fmt.Errorf("write publication plan: %w", err)
	}
	fmt.Printf("action=%s\nreason=%s\nexpectedSourceDigest=%s\nexpectedAliasDigest=%s\n", plan.Action, plan.Reason, plan.ExpectedSourceDigest, plan.ExpectedAliasDigest)
	return nil
}

func promoteCommand(args []string) error {
	flags := flag.NewFlagSet("promote", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	catalogPath := flags.String("catalog", "", "append-only catalog JSON path")
	planPath := flags.String("plan", "", "publication plan JSON path")
	protected := flags.Bool("protected", false, "manually promote a candidate after protected review")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *catalogPath == "" || *planPath == "" {
		return errors.New("promote requires --catalog and --plan")
	}
	catalog, err := readCatalog(*catalogPath)
	if err != nil {
		return err
	}
	var plan prebuilt.PublicationPlan
	if err := readJSON(*planPath, &plan); err != nil {
		return fmt.Errorf("read publication plan: %w", err)
	}
	resolver := prebuilt.RemoteDescriptorResolver{Authenticator: authn.Anonymous}
	publisher := prebuilt.Publisher{Resolver: resolver}
	var promoteErr error
	if *protected {
		promoteErr = publisher.PromoteProtected(context.Background(), &catalog, plan)
	} else {
		promoteErr = publisher.Promote(context.Background(), &catalog, plan)
	}
	if promoteErr != nil {
		return fmt.Errorf("promote publication: %w", promoteErr)
	}
	if err := writeCatalog(*catalogPath, catalog); err != nil {
		return err
	}
	fmt.Printf("promoted action=%s packageIndexDigest=%s\n", plan.Action, plan.Entry.PackageIndexDigest)
	return nil
}

func catalogCommand(args []string) error {
	flags := flag.NewFlagSet("catalog", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	catalogPath := flags.String("catalog", "", "append-only catalog JSON path")
	outputPath := flags.String("output", "", "canonical catalog JSON output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *catalogPath == "" || *outputPath == "" {
		return errors.New("catalog requires --catalog and --output")
	}
	catalog, err := readCatalog(*catalogPath)
	if err != nil {
		return err
	}
	canonical, err := catalog.MarshalCanonical()
	if err != nil {
		return fmt.Errorf("canonicalize catalog: %w", err)
	}
	digest, err := catalog.CatalogDigest()
	if err != nil {
		return err
	}
	if err := atomicWrite(*outputPath, canonical); err != nil {
		return fmt.Errorf("write canonical catalog: %w", err)
	}
	fmt.Printf("catalogDigest=%s\n", digest)
	return nil
}

func verifyCatalogCommand(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repository := flags.String("repository", prebuilt.DefaultPackageRepository, "EPAR package repository")
	profile := flags.String("profile", prebuilt.ProfileAct, "profile alias to resolve")
	issuer := flags.String("issuer", prebuilt.GitHubActionsIssuer, "trusted OIDC issuer")
	workflow := flags.String("workflow", "docker-sandboxes-images.yml", "workflow path")
	ref := flags.String("ref", "refs/heads/main", "trusted workflow ref")
	event := flags.String("event", "", "optional exact workflow event")
	allowedEvents := flags.String("allowed-events", "schedule,workflow_dispatch,push", "comma-separated allowed workflow events")
	immutableReference := flags.String("reference", "", "optional immutable catalog-v1-pkg tag or repo@manifest digest to verify")
	outputPath := flags.String("output", "", "optional canonical catalog JSON output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resolver, err := newCatalogResolver(*repository, *issuer, *workflow, *ref, *event, *allowedEvents)
	if err != nil {
		return err
	}
	var result any
	var canonical []byte
	if strings.TrimSpace(*immutableReference) != "" {
		verified, err := resolver.VerifyCatalogReference(context.Background(), *immutableReference)
		if err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
		digest, err := verified.Artifact.Catalog.CatalogDigest()
		if err != nil {
			return err
		}
		canonical, err = verified.Artifact.Catalog.MarshalCanonical()
		if err != nil {
			return err
		}
		result = struct {
			CatalogReference string `json:"catalogReference"`
			CatalogDigest    string `json:"catalogDigest"`
			CatalogManifest  string `json:"catalogManifestDigest"`
		}{verified.Artifact.Reference, digest, verified.Artifact.ManifestDigest}
	} else {
		resolved, err := resolver.Resolve(context.Background(), *profile)
		if err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
		digest, err := resolved.Catalog.CatalogDigest()
		if err != nil {
			return err
		}
		canonical, err = resolved.Catalog.MarshalCanonical()
		if err != nil {
			return err
		}
		result = struct {
			CatalogReference string         `json:"catalogReference"`
			CatalogDigest    string         `json:"catalogDigest"`
			CatalogManifest  string         `json:"catalogManifestDigest"`
			Profile          string         `json:"profile"`
			Alias            prebuilt.Alias `json:"alias"`
			Entry            prebuilt.Entry `json:"entry"`
		}{resolved.CatalogReference, digest, resolved.CatalogManifest, *profile, resolved.Alias, resolved.Entry}
	}
	if strings.TrimSpace(*outputPath) != "" {
		if err := atomicWrite(*outputPath, append(canonical, '\n')); err != nil {
			return fmt.Errorf("write canonical catalog: %w", err)
		}
	}
	if err := writeJSON("-", result); err != nil {
		return err
	}
	return nil
}

func verifyPackageCommand(args []string) error {
	flags := flag.NewFlagSet("verify-package", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	reference := flags.String("reference", "", "immutable package reference repo@sha256:digest")
	entryPath := flags.String("entry", "", "candidate PublicationPlan or Entry JSON path")
	repository := flags.String("repository", prebuilt.DefaultPackageRepository, "EPAR package repository")
	issuer := flags.String("issuer", prebuilt.GitHubActionsIssuer, "trusted OIDC issuer")
	workflow := flags.String("workflow", "docker-sandboxes-images.yml", "workflow path")
	ref := flags.String("ref", "refs/heads/main", "trusted workflow ref")
	event := flags.String("event", "", "optional exact workflow event")
	allowedEvents := flags.String("allowed-events", "schedule,workflow_dispatch,push", "comma-separated allowed workflow events")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *reference == "" || *entryPath == "" {
		return errors.New("verify-package requires --reference and --entry")
	}
	parts := strings.Split(*reference, "@")
	if len(parts) != 2 {
		return errors.New("verify-package reference must be repo@sha256:digest")
	}
	packageDigest, err := prebuilt.NormalizeDigest(parts[1])
	if err != nil {
		return err
	}
	resolver, err := newCatalogResolver(*repository, *issuer, *workflow, *ref, *event, *allowedEvents)
	if err != nil {
		return err
	}
	var entry prebuilt.Entry
	if err := readEntryOrPlan(*entryPath, &entry); err != nil {
		return fmt.Errorf("read package entry: %w", err)
	}
	verified, err := resolver.VerifyPackage(context.Background(), packageDigest, entry)
	if err != nil {
		return fmt.Errorf("verify-package: %w", err)
	}
	return writeJSON("-", struct {
		PackageDigest string                     `json:"packageDigest"`
		Package       prebuilt.ResolvedReference `json:"package"`
		Evidence      prebuilt.EvidenceResult    `json:"evidence"`
	}{packageDigest, verified.Package, verified.Evidence})
}

func readEntryOrPlan(path string, entry *prebuilt.Entry) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, entry); err == nil && entry.PackageIndexDigest != "" {
		return nil
	}
	var plan prebuilt.PublicationPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return err
	}
	if plan.Entry.PackageIndexDigest == "" {
		return errors.New("entry JSON must contain packageIndexDigest or PublicationPlan.entry")
	}
	*entry = plan.Entry
	return nil
}

func newCatalogResolver(repository, issuer, workflow, ref, event, allowedEvents string) (prebuilt.CatalogResolver, error) {
	events := make([]string, 0)
	for _, value := range strings.Split(allowedEvents, ",") {
		if value = strings.TrimSpace(value); value != "" {
			events = append(events, value)
		}
	}
	policy := prebuilt.EvidencePolicy{Issuer: issuer, Repository: "solutionforest/ephemeral-action-runner", Workflow: workflow, Ref: ref, Event: event, AllowedEvents: events}
	registry := prebuilt.NewRemoteCatalogRegistry()
	evidence, err := prebuilt.NewSigstoreEvidenceVerifier(context.Background())
	if err != nil {
		return prebuilt.CatalogResolver{}, err
	}
	return prebuilt.CatalogResolver{Registry: registry, Evidence: evidence, PackageRepository: repository, EvidencePolicy: policy}, nil
}

func readCatalog(path string) (prebuilt.Catalog, error) {
	var catalog prebuilt.Catalog
	if err := readJSON(path, &catalog); err != nil {
		return catalog, fmt.Errorf("read catalog: %w", err)
	}
	if err := catalog.Validate(); err != nil {
		return catalog, fmt.Errorf("validate catalog: %w", err)
	}
	return catalog, nil
}

func writeCatalog(path string, catalog prebuilt.Catalog) error {
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("validate catalog before write: %w", err)
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("encode catalog: %w", err)
	}
	return atomicWrite(path, append(data, '\n'))
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'))
}

func atomicWrite(path string, data []byte) error {
	if path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".epar-prebuilt-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows cannot atomically rename over an existing destination. Move the
	// old file to a sibling backup, install the fully-synced temp file, and
	// restore the backup if installation fails. The backup is removed only
	// after the new file is in place, so an interrupted replace is recoverable.
	backupPath := tmpPath + ".previous"
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return os.Rename(tmpPath, path)
		}
		return err
	}
	if err := os.Rename(path, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Rename(backupPath, path)
		return err
	}
	_ = os.Remove(backupPath)
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
