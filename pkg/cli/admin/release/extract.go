package release

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	"k8s.io/klog"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	kcmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/openshift/library-go/pkg/image/dockerv1client"
	"github.com/openshift/oc/pkg/cli/image/extract"
	"github.com/openshift/oc/pkg/cli/image/imagesource"
	imagemanifest "github.com/openshift/oc/pkg/cli/image/manifest"
	"github.com/openshift/oc/pkg/cli/image/workqueue"
)

func NewExtractOptions(streams genericclioptions.IOStreams) *ExtractOptions {
	return &ExtractOptions{
		IOStreams: streams,
		Directory: ".",
	}
}

func NewExtract(f kcmdutil.Factory, parentName string, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewExtractOptions(streams)
	cmd := &cobra.Command{
		Use:   "extract",
		Short: "Extract the contents of an update payload to disk",
		Long: templates.LongDesc(`
			Extract the contents of a release image to disk

			Extracts the contents of an OpenShift release image to disk for inspection or
			debugging. Update images contain manifests and metadata about the operators that
			must be installed on the cluster for a given version.

			The --tools and --command flags allow you to extract the appropriate client binaries
			for	your operating system to disk. --tools will create archive files containing the
			current OS tools (or, if --command-os is set to '*', all OS versions). Specifying
			--command for either 'oc' or 'openshift-install' will extract the binaries directly.
			You may pass a PGP private key file with --signing-key which will create an ASCII
			armored sha256sum.txt.asc file describing the content that was extracted that is
			signed by the key. For more advanced signing use the generated sha256sum.txt and an
			external tool like gpg.

			Instead of extracting the manifests, you can specify --git=DIR to perform a Git
			checkout of the source code that comprises the release. A warning will be printed
			if the component is not associated with source code. The command will not perform
			any destructive actions on your behalf except for executing a 'git checkout' which
			may change the current branch. Requires 'git' to be on your path.
		`),
		Example: templates.Examples(fmt.Sprintf(`
			# Use git to check out the source code for the current cluster release to DIR
			%[1]s extract --git=DIR
			`, parentName)),
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(f, cmd, args))
			kcmdutil.CheckErr(o.Run())
		},
	}
	flags := cmd.Flags()
	o.SecurityOptions.Bind(flags)
	o.ParallelOptions.Bind(flags)

	flags.StringVar(&o.From, "from", o.From, "Image containing the release payload.")
	flags.StringVar(&o.File, "file", o.File, "Extract a single file from the payload to standard output.")
	flags.StringVar(&o.Directory, "to", o.Directory, "Directory to write release contents to, defaults to the current directory.")

	flags.StringVar(&o.GitExtractDir, "git", o.GitExtractDir, "Check out the sources that created this release into the provided dir. Repos will be created at <dir>/<host>/<path>. Requires 'git' on your path.")
	flags.BoolVar(&o.Tools, "tools", o.Tools, "Extract the tools archives from the release image. Implies --command=*")
	flags.StringVar(&o.SigningKey, "signing-key", o.SigningKey, "Sign the sha256sum.txt generated by --tools with this GPG key. A sha256sum.txt.asc file signed by this key will be created. The key is assumed to be encrypted.")

	flags.StringVar(&o.Command, "command", o.Command, "Specify 'oc' or 'openshift-install' to extract the client for your operating system.")
	flags.StringVar(&o.CommandOperatingSystem, "command-os", o.CommandOperatingSystem, "Override which operating system command is extracted (mac, windows, linux). You map specify '*' to extract all tool archives.")
	flags.StringVar(&o.FileDir, "dir", o.FileDir, "The directory on disk that file:// images will be copied under.")
	return cmd
}

type ExtractOptions struct {
	genericclioptions.IOStreams

	SecurityOptions imagemanifest.SecurityOptions
	ParallelOptions imagemanifest.ParallelOptions

	From string

	Tools                  bool
	Command                string
	CommandOperatingSystem string
	SigningKey             string

	// GitExtractDir is the path of a root directory to extract the source of a release to.
	GitExtractDir string

	Directory string
	File      string
	FileDir   string

	ImageMetadataCallback func(m *extract.Mapping, dgst, contentDigest digest.Digest, config *dockerv1client.DockerImageConfig)
}

func (o *ExtractOptions) Complete(f kcmdutil.Factory, cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 1 && len(o.From) > 0, len(args) > 1:
		return fmt.Errorf("you may only specify a single image via --from or argument")
	}
	if len(o.From) > 0 {
		args = []string{o.From}
	}
	args, err := findArgumentsFromCluster(f, args)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return fmt.Errorf("you may only specify a single image via --from or argument")
	}
	o.From = args[0]

	return nil
}

func (o *ExtractOptions) Run() error {
	sources := 0
	if o.Tools {
		sources++
	}
	if len(o.File) > 0 {
		sources++
	}
	if len(o.Command) > 0 {
		sources++
	}
	if len(o.GitExtractDir) > 0 {
		sources++
	}

	switch {
	case sources > 1:
		return fmt.Errorf("only one of --tools, --command, --file, or --git may be specified")
	case len(o.From) == 0:
		return fmt.Errorf("must specify an image containing a release payload with --from")
	case o.Directory != "." && len(o.File) > 0:
		return fmt.Errorf("only one of --to and --file may be set")

	case len(o.GitExtractDir) > 0:
		return o.extractGit(o.GitExtractDir)
	case o.Tools:
		return o.extractTools()
	case len(o.Command) > 0:
		return o.extractCommand(o.Command)
	}

	dir := o.Directory
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}

	src := o.From
	ref, err := imagesource.ParseReference(src)
	if err != nil {
		return err
	}
	opts := extract.NewOptions(genericclioptions.IOStreams{Out: o.Out, ErrOut: o.ErrOut})
	opts.SecurityOptions = o.SecurityOptions
	opts.FileDir = o.FileDir

	switch {
	case len(o.File) > 0:
		if o.ImageMetadataCallback != nil {
			opts.ImageMetadataCallback = o.ImageMetadataCallback
		}
		opts.OnlyFiles = true
		opts.Mappings = []extract.Mapping{
			{
				ImageRef: ref,

				From: "release-manifests/",
				To:   dir,
			},
		}
		found := false
		opts.TarEntryCallback = func(hdr *tar.Header, _ extract.LayerInfo, r io.Reader) (bool, error) {
			if hdr.Name != o.File {
				return true, nil
			}
			if _, err := io.Copy(o.Out, r); err != nil {
				return false, err
			}
			found = true
			return false, nil
		}
		if err := opts.Run(); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("image did not contain %s", o.File)
		}
		return nil

	default:
		opts.OnlyFiles = true
		opts.Mappings = []extract.Mapping{
			{
				ImageRef: ref,

				From: "release-manifests/",
				To:   dir,
			},
		}
		verifier := imagemanifest.NewVerifier()
		opts.ImageMetadataCallback = func(m *extract.Mapping, dgst, contentDigest digest.Digest, config *dockerv1client.DockerImageConfig) {
			verifier.Verify(dgst, contentDigest)
			if o.ImageMetadataCallback != nil {
				o.ImageMetadataCallback(m, dgst, contentDigest, config)
			}
			if len(ref.Ref.ID) > 0 {
				fmt.Fprintf(o.Out, "Extracted release payload created at %s\n", config.Created.Format(time.RFC3339))
			} else {
				fmt.Fprintf(o.Out, "Extracted release payload from digest %s created at %s\n", dgst, config.Created.Format(time.RFC3339))
			}
		}
		if err := opts.Run(); err != nil {
			return err
		}
		if !verifier.Verified() {
			err := fmt.Errorf("the release image failed content verification and may have been tampered with")
			if !o.SecurityOptions.SkipVerification {
				return err
			}
			fmt.Fprintf(o.ErrOut, "warning: %v\n", err)
		}
		return nil
	}
}

func (o *ExtractOptions) extractGit(dir string) error {
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}

	opts := NewInfoOptions(o.IOStreams)
	opts.SecurityOptions = o.SecurityOptions
	opts.FileDir = o.FileDir
	release, err := opts.LoadReleaseInfo(o.From, false)
	if err != nil {
		return err
	}

	hadErrors := false
	var once sync.Once
	alreadyExtracted := make(map[string]string)
	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()
	q := workqueue.New(8, ctx.Done())
	q.Batch(func(w workqueue.Work) {
		for _, ref := range release.References.Spec.Tags {
			repo := ref.Annotations[annotationBuildSourceLocation]
			commit := ref.Annotations[annotationBuildSourceCommit]
			if len(repo) == 0 || len(commit) == 0 {
				if klog.V(2) {
					klog.Infof("Tag %s has no source info", ref.Name)
				} else {
					fmt.Fprintf(o.ErrOut, "warning: Tag %s has no source info\n", ref.Name)
				}
				continue
			}
			if oldCommit, ok := alreadyExtracted[repo]; ok {
				if oldCommit != commit {
					fmt.Fprintf(o.ErrOut, "warning: Repo %s referenced more than once with different commits, only checking out the first reference\n", repo)
				}
				continue
			}
			alreadyExtracted[repo] = commit

			w.Parallel(func() {
				buf := &bytes.Buffer{}
				extractedRepo, err := ensureCloneForRepo(dir, repo, nil, buf, buf)
				if err != nil {
					once.Do(func() { hadErrors = true })
					fmt.Fprintf(o.ErrOut, "error: cloning %s: %v\n%s\n", repo, err, buf.String())
					return
				}

				klog.V(2).Infof("Checkout %s from %s ...", commit, repo)
				buf.Reset()
				if err := extractedRepo.CheckoutCommit(repo, commit); err != nil {
					once.Do(func() { hadErrors = true })
					fmt.Fprintf(o.ErrOut, "error: checking out commit for %s: %v\n%s\n", repo, err, buf.String())
					return
				}
				fmt.Fprintf(o.Out, "%s\n", extractedRepo.path)
			})
		}
	})
	if hadErrors {
		return kcmdutil.ErrExit
	}
	return nil
}
