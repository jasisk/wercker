//   Copyright 2016 Wercker Holding BV
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package dockerlocal

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	dockersignal "github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/pkg/term"
	"github.com/docker/go-connections/nat"
	"github.com/fsouza/go-dockerclient"
	"github.com/google/shlex"
	digest "github.com/opencontainers/go-digest"
	"github.com/pborman/uuid"
	"github.com/wercker/docker-check-access"
	"github.com/wercker/wercker/auth"
	"github.com/wercker/wercker/core"
	"github.com/wercker/wercker/util"
	"golang.org/x/net/context"
)

const (
	// DefaultDockerRegistryUsername is an arbitrary value. It is unused by callees,
	// so the value can be anything so long as it's not empty.
	DefaultDockerRegistryUsername = "token"
	DefaultDockerCommand          = `/bin/sh -c "if [ -e /bin/bash ]; then /bin/bash; else /bin/sh; fi"`
)

func RequireDockerEndpoint(options *Options) error {
	client, err := NewDockerClient(options)
	if err != nil {
		if err == docker.ErrInvalidEndpoint {
			return fmt.Errorf(`The given Docker endpoint is invalid:
		  %s
		To specify a different endpoint use the DOCKER_HOST environment variable,
		or the --docker-host command-line flag.
`, options.Host)
		}
		return err
	}
	_, err = client.Version()
	if err != nil {
		if err == docker.ErrConnectionRefused {
			return fmt.Errorf(`You don't seem to have a working Docker environment or wercker can't connect to the Docker endpoint:
	%s
To specify a different endpoint use the DOCKER_HOST environment variable,
or the --docker-host command-line flag.`, options.Host)
		}
		return err
	}
	return nil
}

// GenerateDockerID will generate a cryptographically random 256 bit hex Docker
// identifier.
func GenerateDockerID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// DockerClient is our wrapper for docker.Client
type DockerClient struct {
	*docker.Client
	logger *util.LogEntry
}

// NewDockerClient based on options and env
func NewDockerClient(options *Options) (*DockerClient, error) {
	dockerHost := options.Host
	tlsVerify := options.TLSVerify

	logger := util.RootLogger().WithField("Logger", "Docker")

	var (
		client *docker.Client
		err    error
	)

	if tlsVerify == "1" {
		// We're using TLS, let's locate our certs and such
		// boot2docker puts its certs at...
		dockerCertPath := options.CertPath

		// TODO(termie): maybe fast-fail if these don't exist?
		cert := path.Join(dockerCertPath, fmt.Sprintf("cert.pem"))
		ca := path.Join(dockerCertPath, fmt.Sprintf("ca.pem"))
		key := path.Join(dockerCertPath, fmt.Sprintf("key.pem"))
		client, err = docker.NewVersionnedTLSClient(dockerHost, cert, key, ca, "")
		if err != nil {
			return nil, err
		}
	} else {
		client, err = docker.NewClient(dockerHost)
		if err != nil {
			return nil, err
		}
	}
	return &DockerClient{Client: client, logger: logger}, nil
}

// RunAndAttach gives us a raw connection to a newly run container
func (c *DockerClient) RunAndAttach(name string) error {
	hostConfig := &docker.HostConfig{}
	cmd, _ := shlex.Split(DefaultDockerCommand)
	container, err := c.CreateContainer(
		docker.CreateContainerOptions{
			Name: uuid.NewRandom().String(),
			Config: &docker.Config{
				Image:        name,
				Tty:          true,
				OpenStdin:    true,
				Cmd:          cmd,
				AttachStdin:  true,
				AttachStdout: true,
				AttachStderr: true,
				// NetworkDisabled: b.networkDisabled,
				// Volumes: volumes,
			},
			HostConfig: hostConfig,
		})
	if err != nil {
		return err
	}
	c.StartContainer(container.ID, hostConfig)

	return c.AttachTerminal(container.ID)
}

// AttachInteractive starts an interactive session and runs cmd
func (c *DockerClient) AttachInteractive(containerID string, cmd []string, initialStdin []string) error {

	exec, err := c.CreateExec(docker.CreateExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          cmd,
		Container:    containerID,
	})

	if err != nil {
		return err
	}

	// Dump any initial stdin then go into os.Stdin
	readers := []io.Reader{}
	for _, s := range initialStdin {
		if s != "" {
			readers = append(readers, strings.NewReader(s+"\n"))
		}
	}
	readers = append(readers, os.Stdin)
	stdin := io.MultiReader(readers...)

	// This causes our ctrl-c's to be passed to the stuff in the terminal
	var oldState *term.State
	oldState, err = term.SetRawTerminal(os.Stdin.Fd())
	if err != nil {
		return err
	}
	defer term.RestoreTerminal(os.Stdin.Fd(), oldState)

	// Handle resizes
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, dockersignal.SIGWINCH)
	go func() {
		for range sigchan {
			c.ResizeTTY(exec.ID)
		}
	}()

	err = c.StartExec(exec.ID, docker.StartExecOptions{
		InputStream:  stdin,
		OutputStream: os.Stdout,
		ErrorStream:  os.Stderr,
		Tty:          true,
		RawTerminal:  true,
	})

	return err
}

// ResizeTTY resizes the tty size of docker connection so output looks normal
func (c *DockerClient) ResizeTTY(execID string) error {
	ws, err := term.GetWinsize(os.Stdout.Fd())
	if err != nil {
		c.logger.Debugln("Error getting term size: %s", err)
		return err
	}
	err = c.ResizeExecTTY(execID, int(ws.Height), int(ws.Width))
	if err != nil {
		c.logger.Debugln("Error resizing term: %s", err)
		return err
	}
	return nil
}

// AttachTerminal connects us to container and gives us a terminal
func (c *DockerClient) AttachTerminal(containerID string) error {
	c.logger.Println("Attaching to ", containerID)
	opts := docker.AttachToContainerOptions{
		Container:    containerID,
		Logs:         true,
		Stdin:        true,
		Stdout:       true,
		Stderr:       true,
		Stream:       true,
		InputStream:  os.Stdin,
		ErrorStream:  os.Stderr,
		OutputStream: os.Stdout,
		RawTerminal:  true,
	}

	var oldState *term.State

	oldState, err := term.SetRawTerminal(os.Stdin.Fd())
	if err != nil {
		return err
	}
	defer term.RestoreTerminal(os.Stdin.Fd(), oldState)

	go func() {
		err := c.AttachToContainer(opts)
		if err != nil {
			c.logger.Panicln("attach panic", err)
		}
	}()

	_, err = c.WaitContainer(containerID)
	return err
}

// ExecOne uses docker exec to run a command in the container
func (c *DockerClient) ExecOne(containerID string, cmd []string, output io.Writer) error {
	exec, err := c.CreateExec(docker.CreateExecOptions{
		AttachStdin:  false,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		Cmd:          cmd,
		Container:    containerID,
	})
	if err != nil {
		return err
	}

	err = c.StartExec(exec.ID, docker.StartExecOptions{
		OutputStream: output,
	})
	if err != nil {
		return err
	}

	return nil
}

// DockerScratchPushStep creates a new image based on a scratch tarball and
// pushes it
type DockerScratchPushStep struct {
	*DockerPushStep
}

// NewDockerScratchPushStep constructorama
func NewDockerScratchPushStep(stepConfig *core.StepConfig, options *core.PipelineOptions, dockerOptions *Options) (*DockerScratchPushStep, error) {
	name := "docker-scratch-push"
	displayName := "docker scratch'n'push"
	if stepConfig.Name != "" {
		displayName = stepConfig.Name
	}

	// Add a random number to the name to prevent collisions on disk
	stepSafeID := fmt.Sprintf("%s-%s", name, uuid.NewRandom().String())

	baseStep := core.NewBaseStep(core.BaseStepOptions{
		DisplayName: displayName,
		Env:         &util.Environment{},
		ID:          name,
		Name:        name,
		Owner:       "wercker",
		SafeID:      stepSafeID,
		Version:     util.Version(),
	})

	dockerPushStep := &DockerPushStep{
		BaseStep:      baseStep,
		data:          stepConfig.Data,
		dockerOptions: dockerOptions,
		options:       options,
		logger:        util.RootLogger().WithField("Logger", "DockerScratchPushStep"),
	}

	return &DockerScratchPushStep{DockerPushStep: dockerPushStep}, nil
}

// Execute the scratch-n-push
func (s *DockerScratchPushStep) Execute(ctx context.Context, sess *core.Session) (int, error) {
	// This is clearly only relevant to docker so we're going to dig into the
	// transport internals a little bit to get the container ID
	dt := sess.Transport().(*DockerTransport)
	containerID := dt.containerID

	_, err := s.CollectArtifact(containerID)
	if err != nil {
		return -1, err
	}

	// layer.tar has an extra folder in it so we have to strip it :/
	artifactReader, err := os.Open(s.options.HostPath("layer.tar"))
	if err != nil {
		return -1, err
	}
	defer artifactReader.Close()

	layerFile, err := os.OpenFile(s.options.HostPath("real_layer.tar"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return -1, err
	}
	defer layerFile.Close()

	digester := digest.Canonical.Digester()
	mwriter := io.MultiWriter(layerFile, digester.Hash())

	tr := tar.NewReader(artifactReader)
	tw := tar.NewWriter(mwriter)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// finished the tarball
			break
		}

		if err != nil {
			return -1, err
		}

		// Skip the base dir
		if hdr.Name == "./" {
			continue
		}

		if strings.HasPrefix(hdr.Name, "output/") {
			hdr.Name = hdr.Name[len("output/"):]
		} else if strings.HasPrefix(hdr.Name, "source/") {
			hdr.Name = hdr.Name[len("source/"):]
		}

		if len(hdr.Name) == 0 {
			continue
		}

		tw.WriteHeader(hdr)
		_, err = io.Copy(tw, tr)
		if err != nil {
			return -1, err
		}
	}

	config := &container.Config{
		Cmd:          s.cmd,
		Entrypoint:   s.entrypoint,
		Env:          s.env,
		Hostname:     containerID[:16],
		WorkingDir:   s.workingDir,
		Volumes:      s.volumes,
		ExposedPorts: transformPorts(s.ports),
	}

	// Make the JSON file we need
	t := time.Now()
	base := image.V1Image{
		Architecture: "amd64",
		Container:    containerID,
		ContainerConfig: container.Config{
			Hostname: containerID[:16],
		},
		DockerVersion: "1.10",
		Created:       t,
		OS:            "linux",
		Config:        config,
	}

	imageJSON := image.Image{
		V1Image: base,
		History: []image.History{image.History{Created: t}},
		RootFS: &image.RootFS{
			Type:    "layers",
			DiffIDs: []layer.DiffID{layer.DiffID(digester.Digest())},
		},
	}

	js, err := imageJSON.MarshalJSON()
	if err != nil {
		return -1, err
	}

	hash := sha256.New()
	hash.Write(js)
	layerID := hex.EncodeToString(hash.Sum(nil))

	err = os.MkdirAll(s.options.HostPath("scratch", layerID), 0755)
	if err != nil {
		return -1, err
	}

	layerFile.Close()

	err = os.Rename(layerFile.Name(), s.options.HostPath("scratch", layerID, "layer.tar"))
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(s.options.HostPath("scratch"))

	// VERSION file
	versionFile, err := os.OpenFile(s.options.HostPath("scratch", layerID, "VERSION"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return -1, err
	}
	defer versionFile.Close()

	_, err = versionFile.Write([]byte("1.0"))
	if err != nil {
		return -1, err
	}

	err = versionFile.Sync()
	if err != nil {
		return -1, err
	}

	// json file
	jsonFile, err := os.OpenFile(s.options.HostPath("scratch", layerID, "json"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return -1, err
	}
	defer jsonFile.Close()

	_, err = jsonFile.Write(js)
	if err != nil {
		return -1, err
	}

	err = jsonFile.Sync()
	if err != nil {
		return -1, err
	}

	// repositories file
	repositoriesFile, err := os.OpenFile(s.options.HostPath("scratch", "repositories"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return -1, err
	}
	defer repositoriesFile.Close()

	_, err = repositoriesFile.Write([]byte(fmt.Sprintf(`{"%s":{`, s.authenticator.Repository(s.repository))))
	if err != nil {
		return -1, err
	}

	s.tags = s.buildTags()

	for i, tag := range s.tags {
		_, err = repositoriesFile.Write([]byte(fmt.Sprintf(`"%s":"%s"`, tag, layerID)))
		if err != nil {
			return -1, err
		}
		if i != len(s.tags)-1 {
			_, err = repositoriesFile.Write([]byte{','})
			if err != nil {
				return -1, err
			}
		}
	}

	_, err = repositoriesFile.Write([]byte{'}', '}'})
	err = repositoriesFile.Sync()
	if err != nil {
		return -1, err
	}

	// Build our output tarball and start writing to it
	imageFile, err := os.Create(s.options.HostPath("scratch.tar"))
	if err != nil {
		return -1, err
	}
	defer imageFile.Close()

	err = util.TarPath(imageFile, s.options.HostPath("scratch"))
	if err != nil {
		return -1, err
	}
	imageFile.Close()

	client, err := NewDockerClient(s.dockerOptions)
	if err != nil {
		return 1, err
	}

	// Check the auth
	if !s.dockerOptions.Local {
		check, err := s.authenticator.CheckAccess(s.repository, auth.Push)
		if !check || err != nil {
			s.logger.Errorln("Not allowed to interact with this repository:", s.repository)
			return -1, fmt.Errorf("Not allowed to interact with this repository: %s", s.repository)
		}
	}

	s.repository = s.authenticator.Repository(s.repository)
	s.logger.WithFields(util.LogFields{
		"Repository": s.repository,
		"Tags":       s.tags,
		"Message":    s.message,
	}).Debug("Scratch push to registry")

	// Okay, we can access it, do a docker load to import the image then push it
	loadFile, err := os.Open(s.options.HostPath("scratch.tar"))
	if err != nil {
		return -1, err
	}
	defer loadFile.Close()

	e, err := core.EmitterFromContext(ctx)
	if err != nil {
		return 1, err
	}

	err = client.LoadImage(docker.LoadImageOptions{InputStream: loadFile})
	if err != nil {
		return 1, err
	}

	return s.tagAndPush(layerID, e, client)
}

// CollectArtifact is copied from the build, we use this to get the layer
// tarball that we'll include in the image tarball
func (s *DockerScratchPushStep) CollectArtifact(containerID string) (*core.Artifact, error) {
	artificer := NewArtificer(s.options, s.dockerOptions)

	// Ensure we have the host directory

	artifact := &core.Artifact{
		ContainerID:   containerID,
		GuestPath:     s.options.GuestPath("output"),
		HostPath:      s.options.HostPath("layer"),
		HostTarPath:   s.options.HostPath("layer.tar"),
		ApplicationID: s.options.ApplicationID,
		RunID:         s.options.RunID,
		Bucket:        s.options.S3Bucket,
	}

	sourceArtifact := &core.Artifact{
		ContainerID:   containerID,
		GuestPath:     s.options.BasePath(),
		HostPath:      s.options.HostPath("layer"),
		HostTarPath:   s.options.HostPath("layer.tar"),
		ApplicationID: s.options.ApplicationID,
		RunID:         s.options.RunID,
		Bucket:        s.options.S3Bucket,
	}

	// Get the output dir, if it is empty grab the source dir.
	fullArtifact, err := artificer.Collect(artifact)
	if err != nil {
		if err == util.ErrEmptyTarball {
			fullArtifact, err = artificer.Collect(sourceArtifact)
			if err != nil {
				return nil, err
			}
			return fullArtifact, nil
		}
		return nil, err
	}

	return fullArtifact, nil
}

// DockerPushStep needs to implement IStep
type DockerPushStep struct {
	*core.BaseStep
	options       *core.PipelineOptions
	dockerOptions *Options
	data          map[string]string
	email         string
	env           []string
	stopSignal    string
	builtInPush   bool
	labels        map[string]string
	user          string
	authServer    string
	repository    string
	author        string
	message       string
	tags          []string
	ports         map[docker.Port]struct{}
	volumes       map[string]struct{}
	cmd           []string
	entrypoint    []string
	forceTags     bool
	logger        *util.LogEntry
	workingDir    string
	authenticator auth.Authenticator
}

// NewDockerPushStep is a special step for doing docker pushes
func NewDockerPushStep(stepConfig *core.StepConfig, options *core.PipelineOptions, dockerOptions *Options) (*DockerPushStep, error) {
	name := "docker-push"
	displayName := "docker push"
	if stepConfig.Name != "" {
		displayName = stepConfig.Name
	}

	// Add a random number to the name to prevent collisions on disk
	stepSafeID := fmt.Sprintf("%s-%s", name, uuid.NewRandom().String())

	baseStep := core.NewBaseStep(core.BaseStepOptions{
		DisplayName: displayName,
		Env:         &util.Environment{},
		ID:          name,
		Name:        name,
		Owner:       "wercker",
		SafeID:      stepSafeID,
		Version:     util.Version(),
	})

	return &DockerPushStep{
		BaseStep:      baseStep,
		data:          stepConfig.Data,
		logger:        util.RootLogger().WithField("Logger", "DockerPushStep"),
		options:       options,
		dockerOptions: dockerOptions,
	}, nil
}

func (s *DockerPushStep) configure(env *util.Environment) {
	if email, ok := s.data["email"]; ok {
		s.email = env.Interpolate(email)
	}

	if authServer, ok := s.data["auth-server"]; ok {
		s.authServer = env.Interpolate(authServer)
	}

	if repository, ok := s.data["repository"]; ok {
		s.repository = env.Interpolate(repository)
	}

	if tags, ok := s.data["tag"]; ok {
		splitTags := util.SplitSpaceOrComma(tags)
		interpolatedTags := make([]string, len(splitTags))
		for i, tag := range splitTags {
			interpolatedTags[i] = env.Interpolate(tag)
		}
		s.tags = interpolatedTags
	}

	if author, ok := s.data["author"]; ok {
		s.author = env.Interpolate(author)
	}

	if message, ok := s.data["message"]; ok {
		s.message = env.Interpolate(message)
	}

	if ports, ok := s.data["ports"]; ok {
		iPorts := env.Interpolate(ports)
		parts := util.SplitSpaceOrComma(iPorts)
		portmap := make(map[docker.Port]struct{})
		for _, port := range parts {
			port = strings.TrimSpace(port)
			if !strings.Contains(port, "/") {
				port = port + "/tcp"
			}
			portmap[docker.Port(port)] = struct{}{}
		}
		s.ports = portmap
	}

	if volumes, ok := s.data["volumes"]; ok {
		iVolumes := env.Interpolate(volumes)
		parts := util.SplitSpaceOrComma(iVolumes)
		volumemap := make(map[string]struct{})
		for _, volume := range parts {
			volume = strings.TrimSpace(volume)
			volumemap[volume] = struct{}{}
		}
		s.volumes = volumemap
	}

	if workingDir, ok := s.data["working-dir"]; ok {
		s.workingDir = env.Interpolate(workingDir)
	}

	if cmd, ok := s.data["cmd"]; ok {
		parts, err := shlex.Split(cmd)
		if err == nil {
			s.cmd = parts
		}
	}

	if entrypoint, ok := s.data["entrypoint"]; ok {
		parts, err := shlex.Split(entrypoint)
		if err == nil {
			s.entrypoint = parts
		}
	}

	if envi, ok := s.data["env"]; ok {
		parsedEnv, err := shlex.Split(envi)

		if err == nil {
			interpolatedEnv := make([]string, len(parsedEnv))
			for i, envVar := range parsedEnv {
				interpolatedEnv[i] = env.Interpolate(envVar)
			}
			s.env = interpolatedEnv
		}
	}

	if stopsignal, ok := s.data["stopsignal"]; ok {
		s.stopSignal = env.Interpolate(stopsignal)
	}

	if labels, ok := s.data["labels"]; ok {
		parsedLabels, err := shlex.Split(labels)
		if err == nil {
			labelMap := make(map[string]string)
			for _, labelPair := range parsedLabels {
				pair := strings.Split(labelPair, "=")
				labelMap[env.Interpolate(pair[0])] = env.Interpolate(pair[1])
			}
			s.labels = labelMap
		}
	}

	if user, ok := s.data["user"]; ok {
		s.user = env.Interpolate(user)
	}

	if forceTags, ok := s.data["force-tags"]; ok {
		ft, err := strconv.ParseBool(forceTags)
		if err == nil {
			s.forceTags = ft
		}
	} else {
		s.forceTags = true
	}
}

func (s *DockerPushStep) buildAutherOpts(env *util.Environment) dockerauth.CheckAccessOptions {
	opts := dockerauth.CheckAccessOptions{}
	if username, ok := s.data["username"]; ok {
		opts.Username = env.Interpolate(username)
	}
	if password, ok := s.data["password"]; ok {
		opts.Password = env.Interpolate(password)
	}
	if registry, ok := s.data["registry"]; ok {
		opts.Registry = dockerauth.NormalizeRegistry(env.Interpolate(registry))
	}
	if awsAccessKey, ok := s.data["aws-access-key"]; ok {
		opts.AwsAccessKey = env.Interpolate(awsAccessKey)
	}

	if awsSecretKey, ok := s.data["aws-secret-key"]; ok {
		opts.AwsSecretKey = env.Interpolate(awsSecretKey)
	}

	if awsRegion, ok := s.data["aws-region"]; ok {
		opts.AwsRegion = env.Interpolate(awsRegion)
	}

	if awsAuth, ok := s.data["aws-strict-auth"]; ok {
		auth, err := strconv.ParseBool(awsAuth)
		if err == nil {
			opts.AwsStrictAuth = auth
		}
	}

	if awsRegistryID, ok := s.data["aws-registry-id"]; ok {
		opts.AwsRegistryID = env.Interpolate(awsRegistryID)
	}

	if azureClient, ok := s.data["azure-client-id"]; ok {
		opts.AzureClientID = env.Interpolate(azureClient)
	}

	if azureClientSecret, ok := s.data["azure-client-secret"]; ok {
		opts.AzureClientSecret = env.Interpolate(azureClientSecret)
	}

	if azureSubscriptionID, ok := s.data["azure-subscription-id"]; ok {
		opts.AzureSubscriptionID = env.Interpolate(azureSubscriptionID)
	}

	if azureTenantID, ok := s.data["azure-tenant-id"]; ok {
		opts.AzureTenantID = env.Interpolate(azureTenantID)
	}

	if azureResourceGroupName, ok := s.data["azure-resource-group"]; ok {
		opts.AzureResourceGroupName = env.Interpolate(azureResourceGroupName)
	}

	if azureRegistryName, ok := s.data["azure-registry-name"]; ok {
		opts.AzureRegistryName = env.Interpolate(azureRegistryName)
	}

	if azureLoginServer, ok := s.data["azure-login-server"]; ok {
		opts.AzureLoginServer = env.Interpolate(azureLoginServer)
	}

	// If user use Azure or AWS container registry we don't infer.
	if opts.AzureClientSecret == "" && opts.AwsSecretKey == "" {
		s.repository, opts = InferRegistry(s.repository, opts, s.options)
	}

	// Set user and password automatically if using wercker registry
	if opts.Registry == s.options.WerckerContainerRegistry.String() {
		opts.Username = DefaultDockerRegistryUsername
		opts.Password = s.options.AuthToken
		s.builtInPush = true
	}

	return opts
}

// InferRegistry infers the registry from the repository. If no registry is found
// we fallback to Docker Hub registry.
func InferRegistry(repository string, opts dockerauth.CheckAccessOptions, pipelineOptions *core.PipelineOptions) (string, dockerauth.CheckAccessOptions) {
	if repository == "" {
		repository = pipelineOptions.WerckerContainerRegistry.Host + "/" + pipelineOptions.ApplicationOwnerName + "/" + pipelineOptions.ApplicationName
	}
	// Docker repositories must be lowercase
	repository = strings.ToLower(repository)

	if opts.Registry == "" {
		x, _ := reference.ParseNormalizedNamed(repository)
		domain := reference.Domain(x)

		if domain != "docker.io" {
			reg := &url.URL{Scheme: "https", Host: domain, Path: "/v2"}
			opts.Registry = reg.String() + "/"
		}
	}
	return repository, opts
}

// InitEnv parses our data into our config
func (s *DockerPushStep) InitEnv(env *util.Environment) {
	s.configure(env)
	opts := s.buildAutherOpts(env)
	auther, _ := dockerauth.GetRegistryAuthenticator(opts)
	s.authenticator = auther
}

// Fetch NOP
func (s *DockerPushStep) Fetch() (string, error) {
	// nop
	return "", nil
}

// Execute commits the current container and pushes it to the configured
// registry
func (s *DockerPushStep) Execute(ctx context.Context, sess *core.Session) (int, error) {
	// TODO(termie): could probably re-use the transport's client
	client, err := NewDockerClient(s.dockerOptions)
	if err != nil {
		return 1, err
	}
	e, err := core.EmitterFromContext(ctx)
	if err != nil {
		return 1, err
	}

	s.logger.WithFields(util.LogFields{
		"Repository": s.repository,
		"Tags":       s.tags,
		"Message":    s.message,
	}).Debug("Push to registry")

	// This is clearly only relevant to docker so we're going to dig into the
	// transport internals a little bit to get the container ID
	dt := sess.Transport().(*DockerTransport)
	containerID := dt.containerID

	s.tags = s.buildTags()

	if !s.dockerOptions.Local {
		check, err := s.authenticator.CheckAccess(s.repository, auth.Push)
		if err != nil {
			s.logger.Errorln("Error interacting with this repository:", s.repository, err)
			return -1, fmt.Errorf("Error interacting with this repository: %s %v", s.repository, err)
		}
		if !check {
			return -1, fmt.Errorf("Not allowed to interact with this repository: %s", s.repository)
		}
	}
	s.repository = s.authenticator.Repository(s.repository)
	s.logger.Debugln("Init env:", s.data)

	config := docker.Config{
		Cmd:          s.cmd,
		Entrypoint:   s.entrypoint,
		WorkingDir:   s.workingDir,
		User:         s.user,
		Env:          s.env,
		StopSignal:   s.stopSignal,
		Labels:       s.labels,
		ExposedPorts: s.ports,
		Volumes:      s.volumes,
	}

	commitOpts := docker.CommitContainerOptions{
		Container:  containerID,
		Repository: s.repository,
		Author:     s.author,
		Message:    s.message,
		Run:        &config,
		Tag:        s.tags[0],
	}

	s.logger.Debugln("Commit container:", containerID)
	i, err := client.CommitContainer(commitOpts)
	if err != nil {
		return -1, err
	}

	if s.dockerOptions.CleanupImage {
		defer cleanupImage(s.logger, client, s.repository, s.tags[0])
	}

	s.logger.WithField("Image", i).Debug("Commit completed")
	return s.tagAndPush(i.ID, e, client)
}

func (s *DockerPushStep) buildTags() []string {
	if len(s.tags) == 0 && !s.builtInPush {
		s.tags = []string{"latest"}
	} else if len(s.tags) == 0 && s.builtInPush {
		gitTag := fmt.Sprintf("%s-%s", s.options.GitBranch, s.options.GitCommit)
		s.tags = []string{"latest", gitTag}
	}
	return s.tags
}

func (s *DockerPushStep) tagAndPush(imageID string, e *core.NormalizedEmitter, client *DockerClient) (int, error) {
	// Create a pipe since we want a io.Reader but Docker expects a io.Writer
	r, w := io.Pipe()
	// emitStatuses in a different go routine
	go EmitStatus(e, r, s.options)
	defer w.Close()
	for _, tag := range s.tags {
		tagOpts := docker.TagImageOptions{
			Repo:  s.repository,
			Tag:   tag,
			Force: s.forceTags,
		}
		err := client.TagImage(imageID, tagOpts)
		s.logger.Println("Pushing image for tag ", tag)
		if err != nil {
			s.logger.Errorln("Failed to push:", err)
			return 1, err
		}
		pushOpts := docker.PushImageOptions{
			Name:          s.repository,
			OutputStream:  w,
			RawJSONStream: true,
			Tag:           tag,
		}
		if !s.dockerOptions.Local {
			auth := docker.AuthConfiguration{
				Username: s.authenticator.Username(),
				Password: s.authenticator.Password(),
				Email:    s.email,
			}
			err := client.PushImage(pushOpts, auth)
			if err != nil {
				s.logger.Errorln("Failed to push:", err)
				return 1, err
			}
			s.logger.Println("Pushed container:", s.repository, s.tags)

			if s.dockerOptions.CleanupImage {
				defer cleanupImage(s.logger, client, s.repository, tag)
			}
		}
	}
	return 0, nil
}

func cleanupImage(logger *util.LogEntry, client *DockerClient, repository, tag string) {
	imageName := fmt.Sprintf("%s:%s", repository, tag)
	err := client.RemoveImage(imageName)
	if err != nil {
		logger.
			WithError(err).
			WithField("imageName", imageName).
			Warn("Failed to delete image")
	} else {
		logger.
			WithField("imageName", imageName).
			Debug("Deleted image")
	}
}

// CollectFile NOP
func (s *DockerPushStep) CollectFile(a, b, c string, dst io.Writer) error {
	return nil
}

// CollectArtifact NOP
func (s *DockerPushStep) CollectArtifact(string) (*core.Artifact, error) {
	return nil, nil
}

// ReportPath NOP
func (s *DockerPushStep) ReportPath(...string) string {
	// for now we just want something that doesn't exist
	return uuid.NewRandom().String()
}

// ShouldSyncEnv before running this step = TRUE
func (s *DockerPushStep) ShouldSyncEnv() bool {
	// If disable-sync is set, only sync if it is not true
	if disableSync, ok := s.data["disable-sync"]; ok {
		return disableSync != "true"
	}
	return true
}

func transformPorts(in map[docker.Port]struct{}) map[nat.Port]struct{} {
	result := make(map[nat.Port]struct{})

	for k, v := range in {
		result[nat.Port(k)] = v
	}

	return result
}
