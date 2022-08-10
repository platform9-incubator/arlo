package basecluster

import (
	"bytes"
	"fmt"
	"github.com/arlonproj/arlon/pkg/argocd"
	"github.com/arlonproj/arlon/pkg/gitutils"
	logpkg "github.com/arlonproj/arlon/pkg/log"
	"github.com/go-git/go-billy/v5"
	gogit "github.com/go-git/go-git/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"os"
	"path"
	"sigs.k8s.io/kustomize/kyaml/yaml"
	"text/template"
)

// Prepare checks a cluster API manifest file for problems, and if
// validateOnly is false, outputs a modified copy as modifiedYaml
// if some resources have a namespace that needs removal. If validateOnly is true
// or no modifications are necessary, then modifiedYaml is nil.
// An error is returned if other types of (non-namespace related) issues
// are found in the manifest.
func Prepare(fileName string, validateOnly bool) (clusterName string, modifiedYaml []byte, err error) {
	var buf bytes.Buffer
	dirty := false
	enc := yaml.NewEncoder(&buf)
	bld := resource.NewLocalBuilder()
	opts := resource.FilenameOptions{
		Filenames: []string{fileName},
	}
	res := bld.Unstructured().FilenameParam(false, &opts).Do()
	infos, err := res.Infos()
	if err != nil {
		err = fmt.Errorf("builder failed to run: %s", err)
		return
	}
	for _, info := range infos {
		gvk := info.Object.GetObjectKind().GroupVersionKind()
		if gvk.Kind == "Cluster" {
			if clusterName != "" {
				err = fmt.Errorf("there are 2 or more clusters")
				return
			}
			clusterName = info.Name
		}
		var modified bool
		modified, err = removeNamespaceThenEncode(info.Object, enc)
		if err != nil {
			err = fmt.Errorf("failed to remove namespace or encode object: %s", err)
			return
		}
		if modified {
			dirty = true
		}
	}
	if clusterName == "" {
		return "", nil, fmt.Errorf("failed to find cluster resource")
	}
	if !validateOnly && dirty {
		modifiedYaml = buf.Bytes()
	}
	return
}

// -----------------------------------------------------------------------------

func removeNamespaceThenEncode(obj runtime.Object, enc *yaml.Encoder) (modified bool, err error) {
	log := logpkg.GetLogger()
	unstr := &unstructured.Unstructured{}
	if err := scheme.Scheme.Convert(obj, unstr, nil); err != nil {
		return false, fmt.Errorf("failed to convert object: %s", err)
	}
	ns := unstr.GetNamespace()
	if ns != "" {
		log.V(1).Info("removing namespace",
			"resource", unstr.GetName(), "namespace", ns)
		unstr.SetNamespace("")
		modified = true
	}
	if err := enc.Encode(unstr.Object); err != nil {
		return false, fmt.Errorf("failed to encode object: %s", err)
	}
	return
}

// -----------------------------------------------------------------------------

type KustomizationTemplateParams struct {
	ManifestFileName string
}

func PrepareGitDir(
	config *restclient.Config,
	argocdNs string,
	repoUrl string,
	repoRevision string,
	repoPath string,
) (clusterName string, changed bool, err error) {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", false, fmt.Errorf("failed to get kubernetes client: %s", err)
	}
	repo, tmpDir, auth, err := argocd.CloneRepo(kubeClient, argocdNs,
		repoUrl, repoRevision)
	defer os.RemoveAll(tmpDir)
	if err != nil {
		return "", false, fmt.Errorf("failed to clone repo: %s", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", false, fmt.Errorf("failed to get repo worktree: %s", err)
	}
	fs := wt.Filesystem
	manifestFileName, clusterName, err := prepareDir(fs, repoPath, tmpDir)
	changed, err = gitutils.CommitChanges(tmpDir, wt,
		"prepare base cluster files for "+path.Join(repoPath, manifestFileName))
	if err != nil {
		err = fmt.Errorf("failed to commit changes: %s", err)
		return
	}
	if !changed {
		return
	}
	err = repo.Push(&gogit.PushOptions{
		RemoteName: gogit.DefaultRemoteName,
		Auth:       auth,
		Progress:   nil,
		CABundle:   nil,
	})
	if err != nil {
		err = fmt.Errorf("failed to push to remote repository: %s", err)
		return
	}
	return
}

// -----------------------------------------------------------------------------

// Prepares specified directory to use as base cluster, and returns the
// name of the cluster resource. dirRelPath is the name of the directory
// relative to the file system, and actualFsRootDir is the actual path
// of the file system's root directory on the native file system.
func prepareDir(
	fs billy.Filesystem,
	dirRelPath string,
	actualFsRootDir string,
) (manifestFileName string, clusterName string, err error) {
	var kustomizationFound bool
	var configurationsFound bool
	infos, err := fs.ReadDir(dirRelPath)
	if err != nil {
		err = fmt.Errorf("failed to list repo directory: %s", err)
		return
	}
	for _, info := range infos {
		if info.IsDir() {
			err = fmt.Errorf("found subdirectory: %s", info.Name())
			return
		}
		if info.Name() == "kustomization.yaml" {
			kustomizationFound = true
			continue
		}
		if info.Name() == "configurations.yaml" {
			configurationsFound = true
			continue
		}
		if manifestFileName != "" {
			err = fmt.Errorf("multiple manifests found: (%s, %s)",
				manifestFileName, info.Name())
			return
		}
		manifestFileName = info.Name()
	}
	if manifestFileName == "" {
		err = fmt.Errorf("failed to find base cluster manifest file")
		return
	}
	manifestRelPath := path.Join(dirRelPath, manifestFileName)
	manifestAbsPath := path.Join(actualFsRootDir, manifestRelPath)
	clusterName, modifiedYaml, err := Prepare(manifestAbsPath, false)
	if err != nil {
		err = fmt.Errorf("failed to prepare manifest: %s", err)
		return
	}
	if modifiedYaml != nil {
		// The manifest contains namespaces. Overwrite it with the modified
		// copy that has the namespaces reomoved.
		var file billy.File
		file, err = fs.OpenFile(manifestRelPath, os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			err = fmt.Errorf("failed to open manifest for writing: %s", err)
			return
		}
		_, err = bytes.NewBuffer(modifiedYaml).WriteTo(file)
		_ = file.Close()
		if err != nil {
			err = fmt.Errorf("failed to write to manifest: %s", err)
			return
		}
	}
	if !kustomizationFound {
		var tmpl *template.Template
		tmpl, err = template.New("kust").Parse(kustomizationYamlTemplate)
		if err != nil {
			err = fmt.Errorf("failed to create kustomization template: %s", err)
			return
		}
		var file billy.File
		file, err = fs.Create(path.Join(dirRelPath, "kustomization.yaml"))
		if err != nil {
			err = fmt.Errorf("failed to create kustomization.yaml: %s", err)
			return
		}
		err = tmpl.Execute(file, &KustomizationTemplateParams{manifestFileName})
		_ = file.Close()
		if err != nil {
			err = fmt.Errorf("failed to write to kustomization.yaml: %s", err)
			return
		}
	}
	if !configurationsFound {
		var file billy.File
		file, err = fs.Create(path.Join(dirRelPath, "configurations.yaml"))
		if err != nil {
			err = fmt.Errorf("failed to create configurations.yaml: %s", err)
			return
		}
		_, err = file.Write([]byte(configurationsYaml))
		_ = file.Close()
		if err != nil {
			err = fmt.Errorf("failed to write to configurations.yaml: %s", err)
			return
		}
	}
	return
}