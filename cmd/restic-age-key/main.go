// Package main implements the restic-age-key CLI for managing
// age-based encryption keys in restic repositories.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"slices"

	"github.com/josh/restic-api/api/backend"
	"github.com/josh/restic-api/api/backend/azure"
	"github.com/josh/restic-api/api/backend/b2"
	"github.com/josh/restic-api/api/backend/gs"
	"github.com/josh/restic-api/api/backend/limiter"
	"github.com/josh/restic-api/api/backend/local"
	"github.com/josh/restic-api/api/backend/location"
	"github.com/josh/restic-api/api/backend/rclone"
	"github.com/josh/restic-api/api/backend/rest"
	"github.com/josh/restic-api/api/backend/s3"
	"github.com/josh/restic-api/api/backend/sftp"
	"github.com/josh/restic-api/api/backend/swift"
	"github.com/josh/restic-api/api/crypto"
	"github.com/josh/restic-api/api/repository"
	"github.com/josh/restic-api/api/restic"
	"github.com/josh/restic-api/api/textfile"
	"github.com/restic/chunker"
	"github.com/spf13/cobra"
)

const (
	shortIDLength       = 8
	calibrationDuration = 500 * time.Millisecond
	calibrationMaxMB    = 60
	keyFilePerm         = 0o600
	repoVersion         = 2
	randomKeySize       = 32
	maxKeyAttempts      = 20
)

//nolint:staticcheck // Fatal: prefix mirrors restic's CLI wording for users grepping output.
var (
	errFatalSpecifyRepoFile        = errors.New("Fatal: Please specify repository location (-r or --repository-file)")
	errFatalSpecifyRepo            = errors.New("Fatal: Please specify repository location (-r or --repo)")
	errFatalSpecifyRecipient       = errors.New("Fatal: Please specify recipient (-r or --recipient)")
	errFatalSpecifyRecipientOrFile = errors.New("Fatal: Please specify recipient (--recipient) or recipients file (--recipients-file)")
	errFatalBothRecipientAndFile   = errors.New("Fatal: Cannot specify both --recipient and --recipients-file")
	errFatalEmptyRecipientsFile    = errors.New("Fatal: Recipients file contains no recipients")
	errFatalSpecifyRecipientsFile  = errors.New("Fatal: Please specify recipients file (--recipients-file)")
	errFatalReadRecipientsFile     = errors.New("Fatal: Unable to read recipients file")
)

var (
	errEmptyHostname        = errors.New("hostname is empty")
	errEmptyUsername        = errors.New("username is empty")
	errMasterKeyNotLoaded   = errors.New("repo master key not loaded")
	errSetKeysFailed        = errors.New("failed to set keys")
	errNoIdentityFile       = errors.New("no identity file specified")
	errNoPasswordFound      = errors.New("no password found")
	errAgeEncryptTimeout    = errors.New("timeout exceeded while encrypting key with age")
	errAgeDecryptTimeout    = errors.New("timeout exceeded while decrypting key with age")
	errIdentityCmdTimeout   = errors.New("timeout exceeded while executing identity command")
	errPasswordCmdTimeout   = errors.New("timeout exceeded while executing password command")
	errEmptyPasswordFile    = errors.New("empty password file")
	errEmptyPasswordCommand = errors.New("empty password command output")
	errNoPasswordGiven      = errors.New("no password given")
	errRepoNotExist         = errors.New("repository does not exist: unable to open config file")
)

// constants settable at build time.
var (
	AgeProgram    = "age"
	RcloneProgram = "rclone"
	Version       = "dev"
	Commit        = ""
	Date          = ""
)

type options struct {
	ageProgram        string
	rcloneProgram     string
	repo              string
	fromRepo          string
	password          string
	passwordFile      string
	passwordCommand   string
	identityFile      string
	identityCommand   string
	recipient         string
	recipientsFile    string
	host              string
	user              string
	output            string
	timeout           time.Duration
	dryRun            bool
	chunkerPolynomial string
}

//nolint:funlen // wires every subcommand in one place by design.
func newRootCommand() *cobra.Command {
	opts := defaultOptions()

	cmd := &cobra.Command{
		Use:   "restic-age-key",
		Short: "Manage age-based encryption keys for restic repositories",
		Long: `restic-age-key allows you to manage age-based encryption keys for restic repositories.
It supports listing existing keys, adding new keys, and retrieving passwords.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       Version,
	}

	versionTmpl := "{{.Name}} version {{.Version}}"
	if Commit != "" {
		versionTmpl += "\ncommit: " + Commit
	}
	if Date != "" {
		versionTmpl += "\nbuilt:  " + Date
	}
	cmd.SetVersionTemplate(versionTmpl + "\n")

	cmd.PersistentFlags().StringVar(&opts.ageProgram, "age-program", opts.ageProgram, "path to age binary")
	cmd.PersistentFlags().StringVar(&opts.rcloneProgram, "rclone-program", opts.rcloneProgram, "path to rclone")
	cmd.PersistentFlags().StringVar(&opts.identityFile, "identity-file", opts.identityFile, "age identity file (env: RESTIC_AGE_IDENTITY_FILE)")
	cmd.PersistentFlags().StringVar(&opts.identityCommand, "identity-command", opts.identityCommand, "age identity command (env: RESTIC_AGE_IDENTITY_COMMAND)")
	cmd.PersistentFlags().DurationVar(&opts.timeout, "timeout", opts.timeout, "command timeout (env: RESTIC_AGE_TIMEOUT)")

	addDecryptRepoCommands := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&opts.repo, "repo", opts.repo, "restic repository location (env: RESTIC_REPOSITORY)")
		cmd.Flags().StringVar(&opts.password, "password", opts.password, "restic repository password (env: RESTIC_PASSWORD)")
		cmd.Flags().StringVar(&opts.passwordFile, "password-file", opts.passwordFile, "restic repository password file (env: RESTIC_PASSWORD_FILE)")
		cmd.Flags().StringVar(&opts.passwordCommand, "password-command", opts.passwordCommand, "restic repository password command (env: RESTIC_PASSWORD_COMMAND)")
	}

	runWithTimeout := func(cmd *cobra.Command, run runFunc, args []string) error {
		ctx := cmd.Context()
		if opts.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, opts.timeout)
			defer cancel()
		}
		return run(ctx, opts, args)
	}

	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List all keys in the repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithTimeout(cmd, runKeyList, args)
		},
	}
	addDecryptRepoCommands(listCommand)

	addCommand := &cobra.Command{
		Use:   "add",
		Short: "Add a new key to the repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithTimeout(cmd, runKeyAdd, args)
		},
	}
	addDecryptRepoCommands(addCommand)
	addCommand.Flags().StringVar(&opts.recipient, "recipient", opts.recipient, "age recipient public key (env: RESTIC_AGE_RECIPIENT)")
	addCommand.Flags().StringVar(&opts.host, "host", opts.host, "the hostname for new key")
	addCommand.Flags().StringVar(&opts.user, "user", opts.user, "the username for new key")
	addCommand.Flags().StringVar(&opts.output, "output", "", "output file to write key id to")
	addCommand.Flags().BoolVar(&opts.dryRun, "dry-run", false, "do not add key, just show what would be done")

	setCommand := &cobra.Command{
		Use:   "set",
		Short: "Set keys in the repository based on a recipients file",
		Long:  "Set command adds any pubkeys from the recipients file that aren't in the repo, ignores existing pubkeys, and removes keys from the repo that aren't present in the recipients file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithTimeout(cmd, runKeySet, args)
		},
	}
	addDecryptRepoCommands(setCommand)
	setCommand.Flags().StringVar(&opts.recipientsFile, "recipients-file", opts.recipientsFile, "file containing age recipient public keys (env: RESTIC_AGE_RECIPIENTS_FILE)")
	setCommand.Flags().BoolVar(&opts.dryRun, "dry-run", false, "do not add or remove keys, just show what would be done")

	passwordCommand := &cobra.Command{
		Use:   "password",
		Short: "Retrieve the password for a key",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithTimeout(cmd, runKeyPassword, args)
		},
	}
	passwordCommand.Flags().StringVar(&opts.repo, "repo", opts.repo, "restic repository location (env: RESTIC_REPOSITORY)")
	passwordCommand.Flags().StringVar(&opts.output, "output", "", "output file to write password to")

	fromPasswordCommand := &cobra.Command{
		Use:   "from-password",
		Short: "Retrieve the password for a key",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.repo = opts.fromRepo
			return runWithTimeout(cmd, runKeyPassword, args)
		},
	}
	fromPasswordCommand.Flags().StringVar(&opts.fromRepo, "from-repo", opts.fromRepo, "restic repository location (env: RESTIC_FROM_REPOSITORY)")
	fromPasswordCommand.Flags().StringVar(&opts.output, "output", "", "output file to write password to")

	repoInitCommand := &cobra.Command{
		Use:   "repo-init",
		Short: "Initialize a new repository with an age encrypted key",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithTimeout(cmd, runRepoInit, args)
		},
	}
	repoInitCommand.Flags().StringVarP(&opts.repo, "repo", "r", opts.repo, "repository location (env: RESTIC_REPOSITORY)")
	repoInitCommand.Flags().StringVar(&opts.recipient, "recipient", opts.recipient, "age recipient public key (env: RESTIC_AGE_RECIPIENT)")
	repoInitCommand.Flags().StringVar(&opts.recipientsFile, "recipients-file", opts.recipientsFile, "file containing age recipient public keys (env: RESTIC_AGE_RECIPIENTS_FILE)")
	repoInitCommand.Flags().StringVar(&opts.user, "user", opts.user, "username for key (env: RESTIC_AGE_USER)")
	repoInitCommand.Flags().StringVar(&opts.host, "host", opts.host, "hostname for key (env: RESTIC_AGE_HOST)")
	repoInitCommand.Flags().StringVar(&opts.chunkerPolynomial, "chunker-polynomial", opts.chunkerPolynomial, "chunker polynomial in hex format (e.g. 0x3DA3358B4DC173) (env: RESTIC_AGE_CHUNKER_POLYNOMIAL)")
	repoInitCommand.Flags().StringVar(&opts.output, "output", "", "output file to write key ID to")

	cmd.AddCommand(
		listCommand,
		addCommand,
		setCommand,
		passwordCommand,
		fromPasswordCommand,
		repoInitCommand,
	)

	return cmd
}

type runFunc func(context.Context, options, []string) error

//nolint:cyclop // straight-line env lookups; splitting them adds no clarity.
func defaultOptions() options {
	opts := options{
		ageProgram:        AgeProgram,
		rcloneProgram:     RcloneProgram,
		repo:              os.Getenv("RESTIC_REPOSITORY"),
		fromRepo:          os.Getenv("RESTIC_FROM_REPOSITORY"),
		password:          os.Getenv("RESTIC_PASSWORD"),
		passwordFile:      os.Getenv("RESTIC_PASSWORD_FILE"),
		passwordCommand:   os.Getenv("RESTIC_PASSWORD_COMMAND"),
		identityFile:      os.Getenv("RESTIC_AGE_IDENTITY_FILE"),
		identityCommand:   os.Getenv("RESTIC_AGE_IDENTITY_COMMAND"),
		recipient:         os.Getenv("RESTIC_AGE_RECIPIENT"),
		recipientsFile:    os.Getenv("RESTIC_AGE_RECIPIENTS_FILE"),
		user:              os.Getenv("RESTIC_AGE_USER"),
		host:              os.Getenv("RESTIC_AGE_HOST"),
		chunkerPolynomial: os.Getenv("RESTIC_AGE_CHUNKER_POLYNOMIAL"),
	}

	if timeoutStr := os.Getenv("RESTIC_AGE_TIMEOUT"); timeoutStr != "" {
		if duration, err := time.ParseDuration(timeoutStr); err == nil {
			opts.timeout = duration
		} else {
			fmt.Fprintf(os.Stderr, "warn: invalid timeout format in RESTIC_AGE_TIMEOUT: %s\n", err)
		}
	}

	if opts.host == "" {
		if hostname, err := os.Hostname(); err == nil {
			opts.host = hostname
		}
	}

	if opts.user == "" {
		if u, err := user.Current(); err == nil {
			opts.user = u.Username
		}
	}

	if opts.ageProgram == "" || opts.ageProgram == "age" {
		if path, err := exec.LookPath("age"); err == nil {
			opts.ageProgram = path
		}
	}

	if opts.rcloneProgram == "" || opts.rcloneProgram == "rclone" {
		if path, err := exec.LookPath("rclone"); err == nil {
			opts.rcloneProgram = path
		}
	}

	return opts
}

func main() {
	err := newRootCommand().Execute()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

type AgeKey struct {
	Created  time.Time `json:"created"`
	Username string    `json:"username"`
	Hostname string    `json:"hostname"`

	KDF  string `json:"kdf"`
	N    int    `json:"N"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt []byte `json:"salt"`
	Data []byte `json:"data"`

	AgePubkey string `json:"age-pubkey"`
	AgeData   []byte `json:"age-data"`
}

type Recipient struct {
	ID     restic.ID `json:"-"`
	Pubkey string    `json:"pubkey"`
	Host   string    `json:"host"`
	User   string    `json:"user"`
}

type ListKey struct {
	ID        string
	ShortID   string
	AgePubkey string
	IsCurrent bool
	Username  string
	Hostname  string
	Created   string
}

//nolint:funlen // table rendering reads naturally inline.
func runKeyList(ctx context.Context, opts options, _ []string) error {
	if opts.repo == "" {
		return errFatalSpecifyRepoFile
	}

	repo, _, err := openRepositoryWithPassword(ctx, opts)
	if err != nil {
		return err
	}

	var keys []ListKey

	currentKeyID := repo.KeyID()
	currentKeyIDStr := currentKeyID.String()

	err = repo.List(ctx, restic.KeyFile, func(id restic.ID, _ int64) error {
		data, err := repo.LoadRaw(ctx, restic.KeyFile, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "LoadKey() failed: %v\n", err)
			return nil
		}

		k := &AgeKey{}
		err = json.Unmarshal(data, k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "LoadKey() failed: %v\n", err)
			return nil
		}

		idStr := id.String()
		isCurrent := idStr == currentKeyIDStr

		shortID := idStr
		if len(idStr) > shortIDLength {
			shortID = idStr[:shortIDLength]
		}

		keys = append(keys, ListKey{
			ID:        idStr,
			ShortID:   shortID,
			IsCurrent: isCurrent,
			AgePubkey: k.AgePubkey,
			Username:  k.Username,
			Hostname:  k.Hostname,
			Created:   k.Created.Local().Format("2006-01-02 15:04:05"), //nolint:gosmopolitan // CLI displays local time for users.
		})

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to list repository files: %w", err)
	}

	headers := []string{" ID", "Age Pubkey", "User", "Host", "Created"}
	rows := make([][]string, 0, len(keys))

	for _, key := range keys {
		currentMarker := " "
		if key.IsCurrent {
			currentMarker = "*"
		}

		markedID := currentMarker + key.ShortID

		row := []string{
			markedID,
			key.AgePubkey,
			key.Username,
			key.Hostname,
			key.Created,
		}
		rows = append(rows, row)
	}

	printTable(headers, rows)

	return nil
}

//nolint:cyclop,funlen // step-by-step key construction reads naturally in one function.
func buildAndSaveAgeKey(ctx context.Context, ageProgram, recipient, host, user string, repo *repository.Repository, be backend.Backend, dryRun bool) (restic.ID, error) {
	params, err := crypto.Calibrate(calibrationDuration, calibrationMaxMB)
	if err != nil {
		return restic.ID{}, fmt.Errorf("failed to calibrate crypto parameters: %w", err)
	}

	newkey := &AgeKey{
		Created: time.Now(),
		KDF:     "scrypt",
		N:       params.N,
		R:       params.R,
		P:       params.P,
	}

	newkey.Hostname = host
	if newkey.Hostname == "" {
		return restic.ID{}, errEmptyHostname
	}

	newkey.Username = user
	if newkey.Username == "" {
		return restic.ID{}, errEmptyUsername
	}

	newkey.Salt, err = crypto.NewSalt()
	if err != nil {
		return restic.ID{}, fmt.Errorf("failed to generate new salt: %w", err)
	}

	password, ageData, err := ageEncryptRandomKey(ctx, ageProgram, recipient)
	if err != nil {
		return restic.ID{}, err
	}

	newkey.AgePubkey = recipient
	newkey.AgeData = ageData

	userKey, err := crypto.KDF(params, newkey.Salt, password)
	if err != nil {
		return restic.ID{}, fmt.Errorf("failed to generate key from password: %w", err)
	}

	if repo.Key() == nil {
		return restic.ID{}, errMasterKeyNotLoaded
	}

	buf, err := json.Marshal(repo.Key())
	if err != nil {
		return restic.ID{}, fmt.Errorf("failed to marshal repository key: %w", err)
	}

	nonce := crypto.NewRandomNonce()
	ciphertext := make([]byte, 0, crypto.CiphertextLength(len(buf)))
	ciphertext = append(ciphertext, nonce...)
	ciphertext = userKey.Seal(ciphertext, nonce, buf, nil)
	newkey.Data = ciphertext

	buf, err = json.Marshal(newkey)
	if err != nil {
		return restic.ID{}, fmt.Errorf("failed to marshal new key: %w", err)
	}

	id := restic.Hash(buf)
	h := backend.Handle{Type: restic.KeyFile, Name: id.String()}
	if !dryRun {
		err = be.Save(ctx, h, backend.NewByteReader(buf, be.Hasher()))
		if err != nil {
			return restic.ID{}, fmt.Errorf("failed to save key to backend: %w", err)
		}
	}

	return id, nil
}

func runKeyAdd(ctx context.Context, opts options, _ []string) error {
	repo, be, err := openRepositoryWithPassword(ctx, opts)
	if err != nil {
		return err
	}

	if opts.recipient == "" {
		return errFatalSpecifyRecipient
	}

	id, err := buildAndSaveAgeKey(ctx, opts.ageProgram, opts.recipient, opts.host, opts.user, repo, be, opts.dryRun)
	if err != nil {
		return err
	}

	if opts.dryRun {
		fmt.Fprintf(os.Stderr, "[DRY RUN] Add key %s for %s@%s\n", opts.recipient, opts.user, opts.host)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Add key %s for %s@%s\n", opts.recipient, opts.user, opts.host)

	if opts.output != "" {
		file, err := os.OpenFile(opts.output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, keyFilePerm)
		if err != nil {
			return fmt.Errorf("failed to write key id to file: %w", err)
		}
		defer func() { _ = file.Close() }()

		_, err = file.WriteString(id.String()[:shortIDLength] + "\n")
		if err != nil {
			return fmt.Errorf("failed to write key id to file: %w", err)
		}
	}

	return nil
}

func runKeyPassword(ctx context.Context, opts options, _ []string) error {
	if opts.repo == "" {
		return errFatalSpecifyRepoFile
	}

	password, err := readPasswordViaIdentity(ctx, opts)
	if err != nil {
		return err
	}

	if opts.output != "" {
		file, err := os.OpenFile(opts.output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, keyFilePerm)
		if err != nil {
			return fmt.Errorf("failed to write password to file: %w", err)
		}

		defer func() { _ = file.Close() }()

		if _, err := file.WriteString(password + "\n"); err != nil {
			return fmt.Errorf("failed to write password to file: %w", err)
		}
	} else {
		fmt.Printf("%s\n", password)
	}

	return nil
}

//nolint:gocognit,cyclop,funlen // single-file CLI design; refactoring is tracked in docs/architecture.md.
func runRepoInit(ctx context.Context, opts options, _ []string) error {
	if opts.repo == "" {
		return errFatalSpecifyRepo
	}

	if opts.recipient == "" && opts.recipientsFile == "" {
		return errFatalSpecifyRecipientOrFile
	}

	if opts.recipient != "" && opts.recipientsFile != "" {
		return errFatalBothRecipientAndFile
	}

	pol, err := getChunkerPolynomial(opts)
	if err != nil {
		return fmt.Errorf("failed to get chunker polynomial: %w", err)
	}

	var tempPasswordBuf [randomKeySize]byte
	if _, err := rand.Read(tempPasswordBuf[:]); err != nil {
		return fmt.Errorf("failed to generate temporary password: %w", err)
	}
	tempPassword := hex.EncodeToString(tempPasswordBuf[:])

	repo, be, repoID, err := initializeRepository(ctx, opts, tempPassword, pol)
	if err != nil {
		return err
	}

	if err := repo.SearchKey(ctx, tempPassword, 1, ""); err != nil {
		return fmt.Errorf("failed to load master key: %w", err)
	}

	ageKeyIDs, err := createInitialKeys(ctx, opts, repo, be)
	if err != nil {
		return err
	}

	err = repo.List(ctx, restic.KeyFile, func(id restic.ID, _ int64) error {
		if slices.ContainsFunc(ageKeyIDs, id.Equal) {
			return nil
		}
		h := backend.Handle{Type: restic.KeyFile, Name: id.String()}
		if err := be.Remove(ctx, h); err != nil {
			return fmt.Errorf("failed to remove key %s: %w", id.Str(), err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to remove temporary password key: %w", err)
	}

	fmt.Fprintf(os.Stderr, "created restic repository %s at %s\n", repoID.Str(), opts.repo)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Please note that knowledge of your age identity is required to access")
	fmt.Fprintln(os.Stderr, "the repository. Losing your identity means that your data is")
	fmt.Fprintln(os.Stderr, "irrecoverably lost.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "repository version: 2")

	if opts.recipientsFile != "" {
		recipients, _ := readRecipientsFile(opts.recipientsFile)
		for i, ageKeyID := range ageKeyIDs {
			host := recipients[i].Host
			if host == "" {
				host = opts.host
			}
			user := recipients[i].User
			if user == "" {
				user = opts.user
			}
			fmt.Fprintf(os.Stderr, "  age key %s for %s@%s\n", ageKeyID.Str()[:shortIDLength], user, host)
		}
	} else {
		fmt.Fprintf(os.Stderr, "  age key %s for %s@%s\n", ageKeyIDs[0].Str()[:shortIDLength], opts.user, opts.host)
	}

	if opts.output != "" {
		file, err := os.Create(opts.output)
		if err != nil {
			return fmt.Errorf("failed to create output file: %w", err)
		}
		defer func() { _ = file.Close() }()

		for _, ageKeyID := range ageKeyIDs {
			_, err = file.WriteString(ageKeyID.Str()[:shortIDLength] + "\n")
			if err != nil {
				return fmt.Errorf("failed to write to output file: %w", err)
			}
		}
	}

	return nil
}

func createInitialKeys(ctx context.Context, opts options, repo *repository.Repository, be backend.Backend) ([]restic.ID, error) {
	if opts.recipientsFile == "" {
		ageKeyID, err := buildAndSaveAgeKey(ctx, opts.ageProgram, opts.recipient, opts.host, opts.user, repo, be, false)
		if err != nil {
			return nil, fmt.Errorf("failed to create age key: %w", err)
		}
		return []restic.ID{ageKeyID}, nil
	}

	recipients, err := readRecipientsFile(opts.recipientsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read recipients file: %w", err)
	}

	if len(recipients) == 0 {
		return nil, errFatalEmptyRecipientsFile
	}

	ids := make([]restic.ID, 0, len(recipients))
	for _, recipient := range recipients {
		host := recipient.Host
		if host == "" {
			host = opts.host
		}
		user := recipient.User
		if user == "" {
			user = opts.user
		}
		ageKeyID, err := buildAndSaveAgeKey(ctx, opts.ageProgram, recipient.Pubkey, host, user, repo, be, false)
		if err != nil {
			return nil, fmt.Errorf("failed to create age key for %s: %w", recipient.Pubkey, err)
		}
		ids = append(ids, ageKeyID)
	}
	return ids, nil
}

func parseChunkerPolynomial(hexStr string) (*chunker.Pol, error) {
	if hexStr == "" {
		return nil, nil //nolint:nilnil // empty input means "no polynomial provided"; callers fall back to a random one.
	}

	val, err := strconv.ParseUint(hexStr, 0, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid chunker polynomial: %w", err)
	}

	pol := chunker.Pol(val)
	return &pol, nil
}

func getChunkerPolynomial(opts options) (*chunker.Pol, error) {
	pol, err := parseChunkerPolynomial(opts.chunkerPolynomial)
	if err != nil {
		return nil, err
	}

	if pol != nil {
		return pol, nil
	}

	randomPol, err := chunker.RandomPolynomial()
	if err != nil {
		return nil, fmt.Errorf("failed to generate random polynomial: %w", err)
	}
	return &randomPol, nil
}

//nolint:ireturn // backend.Backend is the only return type the restic backend factory exposes.
func initializeRepository(ctx context.Context, opts options, password string, pol *chunker.Pol) (*repository.Repository, backend.Backend, restic.ID, error) {
	backends := collectBackends()

	loc, err := location.Parse(backends, opts.repo)
	if err != nil {
		return nil, nil, restic.ID{}, fmt.Errorf("failed to parse repository location: %w", err)
	}

	cfg := loc.Config
	if rcloneCfg, ok := cfg.(*rclone.Config); ok {
		rcloneCfg.Program = opts.rcloneProgram
	}

	rt, _ := backend.Transport(backend.TransportOptions{})
	lim := limiter.NewStaticLimiter(limiter.Limits{})
	factory := backends.Lookup(loc.Scheme)

	be, err := factory.Create(ctx, cfg, rt, lim, backendErrorLogf)
	if err != nil {
		return nil, nil, restic.ID{}, fmt.Errorf("failed to create backend: %w", err)
	}

	repo, err := repository.New(be, repository.Options{})
	if err != nil {
		return nil, nil, restic.ID{}, fmt.Errorf("failed to initialize repository: %w", err)
	}

	err = repo.Init(ctx, repoVersion, password, pol)
	if err != nil {
		return nil, nil, restic.ID{}, fmt.Errorf("failed to initialize repository: %w", err)
	}

	repoCfg := repo.Config()

	id, err2 := restic.ParseID(repoCfg.ID)
	if err2 != nil {
		return nil, nil, restic.ID{}, fmt.Errorf("failed to parse repository ID: %w", err2)
	}

	return repo, be, id, nil
}

//nolint:gocognit,cyclop,funlen // single-file CLI design; matches add/remove pairs in one place.
func runKeySet(ctx context.Context, opts options, args []string) error {
	if opts.repo == "" {
		return errFatalSpecifyRepoFile
	}

	if opts.recipientsFile == "" {
		return errFatalSpecifyRecipientsFile
	}

	setRecipients, err := readRecipientsFile(opts.recipientsFile)
	if err != nil {
		return errFatalReadRecipientsFile
	}

	repo, be, err := openRepositoryWithPassword(ctx, opts)
	if err != nil {
		return err
	}

	repoKeys := make(map[string]Recipient)

	err = repo.List(ctx, restic.KeyFile, func(id restic.ID, _ int64) error {
		data, err := repo.LoadRaw(ctx, restic.KeyFile, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "LoadKey() failed: %v\n", err)

			return nil
		}

		k := &AgeKey{}

		err = json.Unmarshal(data, k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "LoadKey() failed: %v\n", err)

			return nil
		}

		if k.AgePubkey != "" {
			repoKeys[k.AgePubkey] = Recipient{
				ID:     id,
				Pubkey: k.AgePubkey,
				Host:   k.Hostname,
				User:   k.Username,
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to list repository files: %w", err)
	}

	var keysToAdd []Recipient
	var keysToRemove []Recipient

	for _, recipient := range setRecipients {
		if _, exists := repoKeys[recipient.Pubkey]; !exists {
			keysToAdd = append(keysToAdd, recipient)
		}
	}

	for pubkey, existingRecipient := range repoKeys {
		found := false
		for _, recipient := range setRecipients {
			if pubkey == recipient.Pubkey {
				found = true
				break
			}
		}
		if !found {
			keysToRemove = append(keysToRemove, existingRecipient)
		}
	}

	logPrefix := ""
	if opts.dryRun {
		logPrefix = "[DRY RUN] "
	}

	hasError := false

	for _, recipient := range keysToAdd {
		addOpts := opts
		addOpts.recipient = recipient.Pubkey
		addOpts.host = recipient.Host
		addOpts.user = recipient.User
		addOpts.dryRun = opts.dryRun

		err := runKeyAdd(ctx, addOpts, args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to add key %s: %v\n", recipient.Pubkey, err)
			hasError = true
		}
	}

	for _, recipient := range keysToRemove {
		if recipient.ID == repo.KeyID() {
			fmt.Fprintf(os.Stderr, "Error: refusing to remove key currently used to access repository\n")
			hasError = true
			continue
		}

		h := backend.Handle{
			Type: restic.KeyFile,
			Name: recipient.ID.String(),
		}

		fmt.Fprintf(os.Stderr, "%sRemove key %s for %s@%s\n", logPrefix, recipient.Pubkey, recipient.User, recipient.Host)

		if opts.dryRun {
			continue
		}

		err := be.Remove(ctx, h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to remove key %s: %v\n", recipient.Pubkey, err)
			hasError = true
		}
	}

	if hasError {
		return errSetKeysFailed
	}

	return nil
}

func readRecipientsFile(path string) ([]Recipient, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from a CLI flag intentionally provided by the user.
	if err != nil {
		return nil, fmt.Errorf("failed to read recipients file: %w", err)
	}

	var recipients []Recipient
	err = json.Unmarshal(data, &recipients)
	if err != nil {
		return nil, fmt.Errorf("failed to parse recipients file as JSON: %w", err)
	}

	return recipients, nil
}

//nolint:cyclop // straight-line iteration over keys + age decrypt; not worth splitting.
func readPasswordViaIdentity(ctx context.Context, opts options) (string, error) {
	repo, _, err := openRepository(ctx, opts)
	if err != nil {
		return "", err
	}

	closeIdentityCommand, err := readIdentityCommand(ctx, &opts)
	if err != nil {
		return "", fmt.Errorf("Resolving identity failed: %w", err) //nolint:staticcheck
	}
	defer closeIdentityCommand()

	if opts.identityFile == "" {
		return "", errNoIdentityFile
	}

	var password string

	err = repo.List(ctx, restic.KeyFile, func(id restic.ID, _ int64) error {
		if password != "" {
			return nil
		}

		data, err := repo.LoadRaw(ctx, restic.KeyFile, id)
		if err != nil {
			return nil //nolint:nilerr // skip unreadable key files; another key may still work.
		}

		k := &AgeKey{}

		err = json.Unmarshal(data, k)
		if err != nil {
			return nil //nolint:nilerr // skip malformed key files; another key may still work.
		}

		if k.AgePubkey == "" {
			return nil
		}

		password, err = ageDecryptKey(ctx, opts.ageProgram, opts.identityFile, k.AgeData)
		if err != nil {
			if strings.Contains(err.Error(), "no identity matched any of the recipients") {
				return nil
			}

			return err
		}

		return nil
	})

	switch {
	case password != "":
		return password, nil
	case err != nil:
		return "", err //nolint:wrapcheck // closure errors already carry context; extra wrap breaks expected stderr.
	default:
		return "", errNoPasswordFound
	}
}

func ageEncryptRandomKey(ctx context.Context, ageProgram string, pubkey string) (string, []byte, error) {
	key := make([]byte, randomKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	cmd := exec.CommandContext(ctx, ageProgram, "--encrypt", "--recipient", pubkey) //nolint:gosec // ageProgram + recipient flag come from this tool's own configuration.
	cmd.Stdin = bytes.NewReader(key)

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", nil, errAgeEncryptTimeout
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil, errors.New(string(exitErr.Stderr)) //nolint:err113 // surfaces age's stderr verbatim to the user.
		}

		return "", nil, fmt.Errorf("failed to encrypt key with age: %w", err)
	}

	return hex.EncodeToString(key), out, nil
}

func ageDecryptKey(ctx context.Context, ageProgram string, identityFile string, key []byte) (string, error) {
	cmd := exec.CommandContext(ctx, ageProgram, "--decrypt", "--identity", identityFile) //nolint:gosec // ageProgram + identity flag come from this tool's own configuration.
	cmd.Stdin = bytes.NewReader(key)

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errAgeDecryptTimeout
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", errors.New(string(exitErr.Stderr)) //nolint:err113 // surfaces age's stderr verbatim to the user.
		}

		return "", fmt.Errorf("failed to decrypt key with age: %w", err)
	}

	return hex.EncodeToString(out), nil
}

func readIdentityCommand(ctx context.Context, opts *options) (func(), error) {
	noop := func() {}

	if opts.identityCommand == "" {
		return noop, nil
	}

	if opts.identityFile != "" {
		fmt.Fprintf(os.Stderr, "warn: ignoring identity-command, identity-file already set\n")

		return noop, nil
	}

	args, err := backend.SplitShellStrings(opts.identityCommand)
	if err != nil {
		return noop, fmt.Errorf("failed to split shell string: %w", err)
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // identity command comes from this tool's own configuration.
	cmd.Stderr = os.Stderr

	output, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return noop, errIdentityCmdTimeout
		}
		return noop, err //nolint:wrapcheck // caller already wraps as "Resolving identity failed: %w".
	}

	filename, closeCallback, err := writeTempFile("identity-*", output)
	if err != nil {
		return closeCallback, err
	}

	opts.identityFile = filename

	return closeCallback, nil
}

func writeTempFile(pattern string, data []byte) (string, func(), error) {
	tmpFile, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary file: %w", err)
	}

	closeCallback := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}

	_, err = tmpFile.Write(data)
	if err != nil {
		closeCallback()

		return "", nil, fmt.Errorf("failed to write to temporary file: %w", err)
	}

	return tmpFile.Name(), closeCallback, nil
}

//nolint:cyclop // switch over four password-input forms is naturally branchy.
func readPassword(ctx context.Context, opts *options) (string, error) {
	switch {
	case opts.password != "":
		return opts.password, nil
	case opts.passwordFile != "":
		s, err := textfile.Read(opts.passwordFile)
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("failed to read password file: %w", err)
		}

		password := strings.TrimSpace(string(s))
		if password == "" {
			return "", errEmptyPasswordFile
		}

		return password, nil
	case opts.passwordCommand != "":
		args, err := backend.SplitShellStrings(opts.passwordCommand)
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}

		cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // password command comes from this tool's own configuration.
		cmd.Stderr = os.Stderr

		output, err := cmd.Output()
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errPasswordCmdTimeout
			}
			return "", fmt.Errorf("failed to execute password command: %w", err)
		}

		password := strings.TrimSpace(string(output))
		if password == "" {
			return "", errEmptyPasswordCommand
		}

		return password, nil
	default:
		return "", errNoPasswordGiven
	}
}

//nolint:ireturn // backend.Backend is the only return type the restic backend factory exposes.
func openRepositoryWithPassword(ctx context.Context, opts options) (*repository.Repository, backend.Backend, error) {
	repo, be, err := openRepository(ctx, opts)
	if err != nil {
		return nil, nil, err
	}

	password, err := readPassword(ctx, &opts)
	if err != nil {
		if opts.identityFile != "" || opts.identityCommand != "" {
			password, err = readPasswordViaIdentity(ctx, opts)
		}

		if err != nil {
			return nil, nil, fmt.Errorf("Fatal: Resolving password failed: %w", err) //nolint:staticcheck
		}
	}

	err = repo.SearchKey(ctx, password, maxKeyAttempts, "")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to verify repository key: %w", err)
	}

	return repo, be, nil
}

//nolint:ireturn // backend.Backend is the only return type the restic backend factory exposes.
func openRepository(ctx context.Context, opts options) (*repository.Repository, backend.Backend, error) {
	backends := collectBackends()

	loc, err := location.Parse(backends, opts.repo)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse repository location: %w", err)
	}

	cfg := loc.Config
	if rcloneCfg, ok := cfg.(*rclone.Config); ok {
		rcloneCfg.Program = opts.rcloneProgram
	}

	rt, _ := backend.Transport(backend.TransportOptions{})
	lim := limiter.NewStaticLimiter(limiter.Limits{})
	factory := backends.Lookup(loc.Scheme)

	be, err := factory.Open(ctx, cfg, rt, lim, backendErrorLogf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open backend: %w", err)
	}

	r, err := repository.New(be, repository.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize repository: %w", err)
	}

	_, err = be.Stat(ctx, backend.Handle{Type: restic.ConfigFile})
	if be.IsNotExist(err) {
		return nil, nil, errRepoNotExist
	}

	return r, be, nil
}

// backendErrorLogf is handed to the backend factories, which call it to report
// out-of-band subprocess output (sftp, rclone). It must never be nil: those
// backends invoke it unconditionally.
func backendErrorLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

func collectBackends() *location.Registry {
	backends := location.NewRegistry()
	backends.Register(azure.NewFactory())
	backends.Register(b2.NewFactory())
	backends.Register(gs.NewFactory())
	backends.Register(local.NewFactory())
	backends.Register(rclone.NewFactory())
	backends.Register(rest.NewFactory())
	backends.Register(s3.NewFactory())
	backends.Register(sftp.NewFactory())
	backends.Register(swift.NewFactory())
	return backends
}

func printTable(headers []string, rows [][]string) {
	padding := 2
	numCols := len(headers)

	colWidths := make([]int, numCols)

	for i, h := range headers {
		if i < numCols && len(h) > colWidths[i] {
			colWidths[i] = len(h)
		}
	}

	for _, row := range rows {
		for i, cell := range row {
			if i < numCols && len(cell) > colWidths[i] {
				colWidths[i] = len(cell)
			}
		}
	}

	totalWidth := 0
	for _, w := range colWidths {
		totalWidth += w
	}
	totalWidth += (numCols - 1) * padding

	printRow(headers, colWidths, padding)
	divider := strings.Repeat("-", totalWidth)
	fmt.Println(divider)

	for _, row := range rows {
		printRow(row, colWidths, padding)
	}

	divider = strings.Repeat("-", totalWidth)
	fmt.Println(divider)
}

func printRow(row []string, colWidths []int, padding int) {
	for i, cell := range row {
		if i >= len(colWidths) {
			break
		}
		fmt.Printf("%-*s", colWidths[i], cell)
		if i < len(colWidths)-1 {
			fmt.Print(strings.Repeat(" ", padding))
		}
	}
	fmt.Println()
}
