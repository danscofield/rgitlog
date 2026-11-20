// rgitlog lists the commits of a remote Git repository, newest first, without
// cloning it to disk.
//
// It talks to the server directly and asks for commit objects only (a
// "tree:0" partial fetch), so no trees or file contents are downloaded.
// Everything it receives is kept in memory and discarded on exit.
//
//	rgitlog git@github.com:owner/repo.git
//	rgitlog -ref v1.2.0 -n 50 https://github.com/owner/repo.git
package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
)

type options struct {
	ref       string
	max       int
	json      bool
	committer bool
	subject   bool
	sshKey    string
	verbose   bool
}

func main() {
	var o options
	flag.StringVar(&o.ref, "ref", "", "branch, tag or full ref name to walk (default: the remote's HEAD)")
	flag.IntVar(&o.max, "n", 0, "stop after `N` commits; the server then only sends the newest N (0 = all)")
	flag.BoolVar(&o.json, "json", false, "print one JSON object per commit instead of tab-separated text")
	flag.BoolVar(&o.committer, "committer", false, "print the committer's email instead of the author's")
	flag.BoolVar(&o.subject, "subject", false, "print only the first line of each commit message")
	flag.StringVar(&o.sshKey, "ssh-key", "", "SSH private key `file` (default: use ssh-agent); passphrase from $SSH_KEY_PASSPHRASE")
	flag.BoolVar(&o.verbose, "v", false, "print transfer statistics to stderr")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up after this long")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: rgitlog [flags] <repository-url>

Lists commits of a remote repository, newest first, as
  <sha> TAB <email> TAB <message>
Newlines, tabs and backslashes in messages are escaped (\n, \t, \\).

Only commit objects are fetched (no trees or file contents), and nothing is
written to disk. URLs can be ssh (git@host:owner/repo.git, ssh://...),
https, http or git://.

ssh uses the same keys ssh would: ssh-agent's, plus ~/.ssh/id_ed25519,
id_ecdsa and id_rsa (or -ssh-key). Host keys are checked against
~/.ssh/known_hosts. For private https repositories set $GIT_TOKEN
(and $GIT_USERNAME if your host needs one).

flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if o.max < 0 {
		fmt.Fprintln(os.Stderr, "rgitlog: -n must not be negative")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, flag.Arg(0), o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "rgitlog:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, url string, o options, out io.Writer) error {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return fmt.Errorf("parsing repository URL: %w", err)
	}
	auth, err := authFor(ep, o.sshKey)
	if err != nil {
		return err
	}
	cli, err := client.NewClient(ep)
	if err != nil {
		return err
	}
	sess, err := cli.NewUploadPackSession(ep, auth)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", ep.Host, err)
	}
	defer sess.Close()

	adv, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return fmt.Errorf("listing remote refs: %w", err)
	}
	tip, refName, err := resolveRef(adv, o.ref)
	if err != nil {
		return err
	}
	if !adv.Capabilities.Supports(capability.Filter) {
		return errors.New("the server does not support partial fetches (the \"filter\" capability), " +
			"so it can't send commits without file contents; refusing to download the full repository")
	}

	req := packp.NewUploadPackRequestFromCapabilities(adv.Capabilities)
	req.Wants = []plumbing.Hash{tip}
	if err := req.Capabilities.Set(capability.Filter); err != nil {
		return err
	}
	req.Filter = packp.FilterTreeDepth(0) // commits only: no trees, no blobs
	if o.max > 0 {
		req.Depth = packp.DepthCommits(o.max)
		if err := req.Capabilities.Set(capability.Shallow); err != nil {
			return err
		}
	}
	if adv.Capabilities.Supports(capability.NoProgress) {
		if err := req.Capabilities.Set(capability.NoProgress); err != nil {
			return err
		}
	}

	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		return fmt.Errorf("fetching commits: %w", err)
	}
	defer resp.Close()

	counted := &countingReader{r: demux(req.Capabilities, resp)}
	store := memory.NewStorage() // in memory only; discarded when the process exits
	if err := packfile.UpdateObjectStorage(store, counted); err != nil {
		return fmt.Errorf("reading commits: %w", err)
	}
	if o.verbose {
		printStats(store, refName, tip, counted.n)
	}

	return walk(store, tip, o, out)
}

// authFor picks credentials for the URL's protocol. Nothing is cached.
func authFor(ep *transport.Endpoint, sshKey string) (transport.AuthMethod, error) {
	switch ep.Protocol {
	case "ssh":
		user := ep.User
		if user == "" {
			user = "git"
		}
		// Host keys are checked against ~/.ssh/known_hosts (or $SSH_KNOWN_HOSTS).
		if sshKey != "" {
			return gitssh.NewPublicKeysFromFile(user, sshKey, os.Getenv("SSH_KEY_PASSPHRASE"))
		}
		return &gitssh.PublicKeysCallback{User: user, Callback: defaultSSHSigners}, nil
	case "http", "https":
		if token := os.Getenv("GIT_TOKEN"); token != "" {
			user := os.Getenv("GIT_USERNAME")
			if user == "" {
				user = "git" // GitHub and GitLab accept any non-empty name with a token
			}
			return &githttp.BasicAuth{Username: user, Password: token}, nil
		}
		if ep.User != "" {
			return &githttp.BasicAuth{Username: ep.User, Password: ep.Password}, nil
		}
		return nil, nil
	case "git":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q: use ssh, https, http or git", ep.Protocol)
	}
}

// defaultSSHSigners offers the same keys `ssh` would by default: whatever
// ssh-agent holds, plus ~/.ssh/id_ed25519, id_ecdsa and id_rsa. Encrypted
// key files are used only if $SSH_KEY_PASSPHRASE is set.
func defaultSSHSigners() ([]ssh.Signer, error) {
	var signers []ssh.Signer
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			// The connection stays open: agent signers sign through it.
			if s, err := agent.NewClient(conn).Signers(); err == nil {
				signers = append(signers, s...)
			}
		}
	}

	home, err := os.UserHomeDir()
	if err == nil {
		passphrase := os.Getenv("SSH_KEY_PASSPHRASE")
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			pem, err := os.ReadFile(filepath.Join(home, ".ssh", name))
			if err != nil {
				continue
			}
			var s ssh.Signer
			if passphrase != "" {
				s, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(passphrase))
			} else {
				s, err = ssh.ParsePrivateKey(pem)
			}
			if err == nil {
				signers = append(signers, s)
			}
		}
	}

	if len(signers) == 0 {
		return nil, errors.New("no usable SSH keys: ssh-agent has none, and no unencrypted " +
			"~/.ssh/id_ed25519, id_ecdsa or id_rsa was found (use -ssh-key, or set $SSH_KEY_PASSPHRASE)")
	}
	return signers, nil
}

// resolveRef finds the commit to start from. For annotated tags it uses the
// peeled commit the server advertises, so the tag object itself isn't needed.
func resolveRef(adv *packp.AdvRefs, want string) (plumbing.Hash, string, error) {
	if want == "" {
		if adv.Head == nil {
			return plumbing.ZeroHash, "", errors.New("the remote has no HEAD; pass -ref to choose a branch or tag")
		}
		return *adv.Head, "HEAD", nil
	}

	candidates := []string{want}
	if !strings.HasPrefix(want, "refs/") {
		candidates = append(candidates, "refs/heads/"+want, "refs/tags/"+want)
	}
	for _, name := range candidates {
		hash, ok := adv.References[name]
		if !ok {
			continue
		}
		if peeled, ok := adv.Peeled[name]; ok {
			return peeled, name, nil
		}
		return hash, name, nil
	}
	return plumbing.ZeroHash, "", fmt.Errorf("ref %q not found on the remote", want)
}

// walk prints commits newest first by committer time, like `git log`. Parents
// that weren't fetched (the history cut off by -n) end the walk on that line.
func walk(s storer.EncodedObjectStorer, tip plumbing.Hash, o options, out io.Writer) error {
	start, err := object.GetCommit(s, tip)
	if err != nil {
		return fmt.Errorf("the server didn't send the starting commit %s: %w", tip, err)
	}

	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	queue := &byCommitTime{start}
	seen := map[plumbing.Hash]bool{start.Hash: true}
	printed := 0

	for queue.Len() > 0 {
		c := heap.Pop(queue).(*object.Commit)
		if err := printCommit(c, o, out, enc); err != nil {
			return err
		}
		printed++
		if o.max > 0 && printed >= o.max {
			return nil
		}
		for _, parent := range c.ParentHashes {
			if seen[parent] {
				continue
			}
			seen[parent] = true
			pc, err := object.GetCommit(s, parent)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				continue // beyond a shallow boundary
			}
			if err != nil {
				return fmt.Errorf("reading commit %s: %w", parent, err)
			}
			heap.Push(queue, pc)
		}
	}
	return nil
}

func printCommit(c *object.Commit, o options, out io.Writer, enc *json.Encoder) error {
	email := c.Author.Email
	if o.committer {
		email = c.Committer.Email
	}
	msg := strings.TrimRight(c.Message, "\n")
	if o.subject {
		msg, _, _ = strings.Cut(msg, "\n")
	}

	if o.json {
		return enc.Encode(struct {
			SHA     string `json:"sha"`
			Email   string `json:"email"`
			Message string `json:"message"`
		}{c.Hash.String(), email, msg})
	}
	_, err := fmt.Fprintf(out, "%s\t%s\t%s\n", c.Hash, escape(email), escape(msg))
	return err
}

var escaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`)

func escape(s string) string { return escaper.Replace(s) }

// demux strips the side-band framing the server wraps the pack in, if any.
func demux(caps *capability.List, r io.Reader) io.Reader {
	switch {
	case caps.Supports(capability.Sideband64k):
		return sideband.NewDemuxer(sideband.Sideband64k, r)
	case caps.Supports(capability.Sideband):
		return sideband.NewDemuxer(sideband.Sideband, r)
	default:
		return r
	}
}

func printStats(s *memory.Storage, refName string, tip plumbing.Hash, bytes int64) {
	counts := map[plumbing.ObjectType]int{}
	for _, t := range []plumbing.ObjectType{plumbing.CommitObject, plumbing.TreeObject, plumbing.BlobObject, plumbing.TagObject} {
		iter, err := s.IterEncodedObjects(t)
		if err != nil {
			continue
		}
		_ = iter.ForEach(func(plumbing.EncodedObject) error { counts[t]++; return nil })
	}
	fmt.Fprintf(os.Stderr, "rgitlog: %s is %s; received %d bytes: %d commits, %d trees, %d blobs, %d tags\n",
		refName, tip, bytes, counts[plumbing.CommitObject], counts[plumbing.TreeObject],
		counts[plumbing.BlobObject], counts[plumbing.TagObject])
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// byCommitTime is a max-heap of commits ordered by committer time.
type byCommitTime []*object.Commit

func (h byCommitTime) Len() int { return len(h) }
func (h byCommitTime) Less(i, j int) bool {
	return h[i].Committer.When.After(h[j].Committer.When)
}
func (h byCommitTime) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *byCommitTime) Push(x any)   { *h = append(*h, x.(*object.Commit)) }
func (h *byCommitTime) Pop() any {
	old := *h
	c := old[len(old)-1]
	*h = old[:len(old)-1]
	return c
}
