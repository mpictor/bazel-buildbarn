package builder

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path"
	"syscall"
	"time"

	"github.com/EdSchouten/bazel-buildbarn/pkg/cas"
	"github.com/EdSchouten/bazel-buildbarn/pkg/util"
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/golang/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	pathTempRoot  = "/tmp"
	pathBuildRoot = "/build"
	pathStdout    = "/stdout"
	pathStderr    = "/stderr"
)

type BuildUser struct {
	Uid, Gid uint32
}

var (
	localBuildExecutorDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "buildbarn",
			Subsystem: "builder",
			Name:      "local_build_executor_duration_seconds",
			Help:      "Amount of time spent per build execution step, in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.001, math.Pow(10.0, 1.0/3.0), 6*3+1),
		},
		[]string{"step"})
)

func init() {
	prometheus.MustRegister(localBuildExecutorDurationSeconds)
}

func joinPathSafe(elem ...string) (string, error) {
	joined := path.Join(elem...)
	if joined != path.Clean(joined) {
		return "", fmt.Errorf("Attempted to access non-clean path %s", joined)
	}
	return joined, nil
}

type localBuildExecutor struct {
	contentAddressableStorage   cas.ContentAddressableStorage
	pathTempRoot, pathBuildRoot string
	pathStdout, pathStderr      string
	user                        *BuildUser
}

// NewLocalBuildExecutor returns a BuildExecutor that executes build
// steps on the local system.
func NewLocalBuildExecutor(user *BuildUser, pathPrefix string, contentAddressableStorage cas.ContentAddressableStorage) BuildExecutor {
	return &localBuildExecutor{
		contentAddressableStorage: contentAddressableStorage,
		pathTempRoot:              path.Join(pathPrefix, pathTempRoot),
		pathBuildRoot:             path.Join(pathPrefix, pathBuildRoot),
		pathStdout:                path.Join(pathPrefix, pathStdout),
		pathStderr:                path.Join(pathPrefix, pathStderr),
		user:                      user,
	}
}

func (be *localBuildExecutor) createInputDirectory(ctx context.Context, digest *util.Digest, base string) error {
	if err := os.Mkdir(base, 0777); err != nil {
		return err
	}

	directory, err := be.contentAddressableStorage.GetDirectory(ctx, digest)
	if err != nil {
		return err
	}

	for _, file := range directory.Files {
		childDigest, err := digest.NewDerivedDigest(file.Digest)
		if err != nil {
			return err
		}
		childPath, err := joinPathSafe(base, file.Name)
		if err != nil {
			return err
		}
		if err := be.contentAddressableStorage.GetFile(ctx, childDigest, childPath, file.IsExecutable); err != nil {
			return err
		}
	}
	for _, directory := range directory.Directories {
		childDigest, err := digest.NewDerivedDigest(directory.Digest)
		if err != nil {
			return err
		}
		childPath, err := joinPathSafe(base, directory.Name)
		if err != nil {
			return err
		}
		if err := be.createInputDirectory(ctx, childDigest, childPath); err != nil {
			return err
		}
	}
	// TODO(edsch): Create symlinks in the input root in a secure way.
	if len(directory.Symlinks) > 0 {
		return errors.New("Creating symlinks in the input root is not yet supported")
	}
	return nil
}

func (be *localBuildExecutor) prepareFilesystem(ctx context.Context, request *remoteexecution.ExecuteRequest, inputRootDigest *util.Digest, command *remoteexecution.Command) error {
	// Copy input files into build environment.
	os.RemoveAll(be.pathBuildRoot)
	if err := be.createInputDirectory(ctx, inputRootDigest, be.pathBuildRoot); err != nil {
		return err
	}

	// Ensure that directories where output files are stored are present.
	for _, outputDirectory := range command.OutputDirectories {
		outputPath, err := joinPathSafe(pathBuildRoot, outputDirectory)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path.Dir(outputPath), 0777); err != nil {
			return err
		}
	}
	for _, outputFile := range command.OutputFiles {
		outputPath, err := joinPathSafe(be.pathBuildRoot, outputFile)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path.Dir(outputPath), 0777); err != nil {
			return err
		}
	}

	// Provide a clean temp directory.
	os.RemoveAll(be.pathTempRoot)
	return os.Mkdir(be.pathTempRoot, 0777)
}

func (be *localBuildExecutor) runCommand(ctx context.Context, command *remoteexecution.Command) error {
	// Prepare the command to run.
	if len(command.Arguments) < 1 {
		return errors.New("Insufficent number of command arguments")
	}
	cmd := exec.CommandContext(ctx, command.Arguments[0], command.Arguments[1:]...)
	workingDirectory, err := joinPathSafe(be.pathBuildRoot, command.WorkingDirectory)
	if err != nil {
		return err
	}
	cmd.Dir = workingDirectory
	cmd.Env = []string{"HOME=" + be.pathTempRoot}
	for _, environmentVariable := range command.EnvironmentVariables {
		cmd.Env = append(cmd.Env, environmentVariable.Name+"="+environmentVariable.Value)
	}

	// Output streams.
	stdout, err := os.OpenFile(be.pathStdout, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer stdout.Close()
	cmd.Stdout = stdout
	stderr, err := os.OpenFile(be.pathStderr, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	cmd.Stderr = stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: be.user.Uid,
			Gid: be.user.Gid,
		},
	}
	return cmd.Run()
}

func (be *localBuildExecutor) uploadDirectory(ctx context.Context, basePath string, parentDigest *util.Digest, permitNonExistent bool, children map[string]*remoteexecution.Directory) (*remoteexecution.Directory, error) {
	files, err := ioutil.ReadDir(basePath)
	if err != nil {
		if permitNonExistent && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var directory remoteexecution.Directory
	for _, file := range files {
		name := file.Name()
		fullPath := path.Join(basePath, name)
		switch file.Mode() & os.ModeType {
		case 0:
			digest, isExecutable, err := be.contentAddressableStorage.PutFile(ctx, fullPath, parentDigest)
			if err != nil {
				return nil, err
			}
			directory.Files = append(directory.Files, &remoteexecution.FileNode{
				Name:         name,
				Digest:       digest.GetRawDigest(),
				IsExecutable: isExecutable,
			})
		case os.ModeDir:
			child, err := be.uploadDirectory(ctx, fullPath, parentDigest, false, children)
			if err != nil {
				return nil, err
			}

			// Compute digest of the child directory. This requires serializing it.
			data, err := proto.Marshal(child)
			if err != nil {
				return nil, err
			}
			digestGenerator := parentDigest.NewDigestGenerator()
			if _, err := digestGenerator.Write(data); err != nil {
				return nil, err
			}
			digest := digestGenerator.Sum()

			children[digest.GetKey(util.DigestKeyWithoutInstance)] = child
			directory.Directories = append(directory.Directories, &remoteexecution.DirectoryNode{
				Name:   name,
				Digest: digest.GetRawDigest(),
			})
		case os.ModeSymlink:
			target, err := os.Readlink(fullPath)
			if err != nil {
				return nil, err
			}
			directory.Symlinks = append(directory.Symlinks, &remoteexecution.SymlinkNode{
				Name:   name,
				Target: target,
			})
		default:
			return nil, fmt.Errorf("Path %s has an unsupported file type", basePath)
		}
	}
	return &directory, nil
}

func (be *localBuildExecutor) uploadTree(ctx context.Context, path string, parentDigest *util.Digest) (*util.Digest, error) {
	// Gather all individual directory objects and turn them into a tree.
	children := map[string]*remoteexecution.Directory{}
	root, err := be.uploadDirectory(ctx, path, parentDigest, true, children)
	if root == nil || err != nil {
		return nil, err
	}
	tree := &remoteexecution.Tree{
		Root: root,
	}
	for _, child := range children {
		tree.Children = append(tree.Children, child)
	}
	return be.contentAddressableStorage.PutTree(ctx, tree, parentDigest)
}

func (be *localBuildExecutor) Execute(ctx context.Context, request *remoteexecution.ExecuteRequest) (*remoteexecution.ExecuteResponse, bool) {
	timeStart := time.Now()

	// Fetch action and command.
	actionDigest, err := util.NewDigest(request.InstanceName, request.ActionDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	action, err := be.contentAddressableStorage.GetAction(ctx, actionDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	commandDigest, err := actionDigest.NewDerivedDigest(action.CommandDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	command, err := be.contentAddressableStorage.GetCommand(ctx, commandDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	timeAfterGetActionCommand := time.Now()
	localBuildExecutorDurationSeconds.WithLabelValues("get_action_command").Observe(
		timeAfterGetActionCommand.Sub(timeStart).Seconds())

	// Set up inputs.
	inputRootDigest, err := actionDigest.NewDerivedDigest(action.InputRootDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	if err := be.prepareFilesystem(ctx, request, inputRootDigest, command); err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	timeAfterPrepareFilesytem := time.Now()
	localBuildExecutorDurationSeconds.WithLabelValues("prepare_filesystem").Observe(
		timeAfterPrepareFilesytem.Sub(timeAfterGetActionCommand).Seconds())

	// Invoke command.
	exitCode := 0
	if err := be.runCommand(ctx, command); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus := exitError.Sys().(syscall.WaitStatus)
			exitCode = waitStatus.ExitStatus()
		} else {
			return convertErrorToExecuteResponse(err), false
		}
	}
	timeAfterRunCommand := time.Now()
	localBuildExecutorDurationSeconds.WithLabelValues("run_command").Observe(
		timeAfterRunCommand.Sub(timeAfterPrepareFilesytem).Seconds())

	response := &remoteexecution.ExecuteResponse{
		Result: &remoteexecution.ActionResult{
			ExitCode: int32(exitCode),
		},
	}

	// Upload command output. In the common case, the files are
	// empty. If that's the case, don't bother setting the digest to
	// keep the ActionResult small.
	stdoutDigest, _, err := be.contentAddressableStorage.PutFile(ctx, be.pathStdout, inputRootDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	if stdoutDigest.GetSizeBytes() > 0 {
		response.Result.StdoutDigest = stdoutDigest.GetRawDigest()
	}
	stderrDigest, _, err := be.contentAddressableStorage.PutFile(ctx, be.pathStderr, inputRootDigest)
	if err != nil {
		return convertErrorToExecuteResponse(err), false
	}
	if stderrDigest.GetSizeBytes() > 0 {
		response.Result.StderrDigest = stderrDigest.GetRawDigest()
	}

	// Upload output files.
	for _, outputFile := range command.OutputFiles {
		outputPath, err := joinPathSafe(be.pathBuildRoot, outputFile)
		if err != nil {
			return convertErrorToExecuteResponse(err), false
		}
		digest, isExecutable, err := be.contentAddressableStorage.PutFile(ctx, outputPath, inputRootDigest)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return convertErrorToExecuteResponse(err), false
		}
		response.Result.OutputFiles = append(response.Result.OutputFiles, &remoteexecution.OutputFile{
			Path:         outputFile,
			Digest:       digest.GetRawDigest(),
			IsExecutable: isExecutable,
		})
	}

	// Upload output directories.
	for _, outputDirectory := range command.OutputDirectories {
		outputPath, err := joinPathSafe(be.pathBuildRoot, outputDirectory)
		if err != nil {
			return convertErrorToExecuteResponse(err), false
		}
		digest, err := be.uploadTree(ctx, outputPath, inputRootDigest)
		if err != nil {
			return convertErrorToExecuteResponse(err), false
		}
		if digest != nil {
			response.Result.OutputDirectories = append(response.Result.OutputDirectories, &remoteexecution.OutputDirectory{
				Path:       outputDirectory,
				TreeDigest: digest.GetRawDigest(),
			})
		}
	}

	// TODO(edsch): Output symlinks.

	timeAfterUpload := time.Now()
	localBuildExecutorDurationSeconds.WithLabelValues("upload_output").Observe(
		timeAfterUpload.Sub(timeAfterRunCommand).Seconds())

	return response, !action.DoNotCache && response.Result.ExitCode == 0
}
