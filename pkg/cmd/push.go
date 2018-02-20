package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strings"

	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"kope.io/build/pkg/docker"
	"kope.io/build/pkg/imageconfig"
	"kope.io/build/pkg/layers"
)

type PushOptions struct {
	Source string
	Dest   string
}

func BuildPushCommand(f Factory, out io.Writer) *cobra.Command {
	options := &PushOptions{}

	cmd := &cobra.Command{
		Use: "push",
		Run: func(cmd *cobra.Command, args []string) {
			options.Source = cmd.Flags().Arg(0)
			options.Dest = cmd.Flags().Arg(1)
			if err := RunPushCommand(f, options, out); err != nil {
				ExitWithError(err)
			}
		},
	}

	return cmd
}

type DockerImageSpec struct {
	Host       string
	Repository string
	Tag        string
}

func (s *DockerImageSpec) String() string {
	v := "docker://"
	if s.Host != "" {
		host := strings.TrimPrefix(s.Host, "https://")
		host = strings.TrimPrefix(host, "http://")
		v += host + "/"
	}
	v += s.Repository + ":" + s.Tag
	return v
}

func ParseDockerImageSpec(s string) (*DockerImageSpec, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("error parsing source name %s: %v", s, err)
	}

	if u.Scheme != "docker" {
		return nil, fmt.Errorf("unknown scheme %q - try e.g. docker://ubuntu:14.04", s)
	}

	v := u.Host
	if u.Path != "" {
		v += u.Path
	}

	spec := &DockerImageSpec{}

	tokens := strings.Split(v, ":")
	if len(tokens) == 1 {
		spec.Tag = "latest"
	} else if len(tokens) == 2 {
		spec.Tag = tokens[1]
	} else {
		return nil, fmt.Errorf("unknown docker image format %q", s)
	}

	tokens = strings.Split(tokens[0], "/")
	if len(tokens) == 1 {
		spec.Repository = "library/" + tokens[0]
	} else if len(tokens) == 2 {
		spec.Repository = tokens[0] + "/" + tokens[1]
	} else if len(tokens) == 3 {
		spec.Host = "https://" + tokens[0]
		spec.Repository = tokens[1] + "/" + tokens[2]
	} else {
		return nil, fmt.Errorf("unknown docker image format %q", s)
	}

	return spec, nil
}

func RunPushCommand(factory Factory, flags *PushOptions, out io.Writer) error {
	if flags.Source == "" {
		return fmt.Errorf("source is required")
	}
	if flags.Dest == "" {
		return fmt.Errorf("dest is required")
	}

	layerStore, err := factory.LayerStore()
	if err != nil {
		return err
	}

	dest, err := ParseDockerImageSpec(flags.Dest)
	if err != nil {
		return err
	}

	targetRegistry := &docker.Registry{
		URL: dest.Host,
	}
	auth := &docker.Auth{}
	//auth := docker.Auth{Subject: dest.Host}
	// token, err := auth.GetToken("repository:" + dest.Repository + ":pull,push")
	//if err != nil {
	//	return fmt.Errorf("error getting registry token: %v", err)
	//}

	var newLayers []*imageconfig.AddLayer
	var baseImage string

	{
		source := flags.Source
		for {
			layer, err := layerStore.FindLayer(source)
			if err != nil {
				return err
			}
			if layer == nil {
				return fmt.Errorf("layer %q not found", source)
			}

			newLayer := &imageconfig.AddLayer{
				Layer: layer,
			}
			// Insert new layer at front
			newLayers = append([]*imageconfig.AddLayer{newLayer}, newLayers...)

			newLayer.Description = fmt.Sprintf("imagebuilder: layer %s", source)

			options, err := layer.GetOptions()
			if err != nil {
				return err
			}
			newLayer.Options = options

			if options.Base == "" || strings.Contains(options.Base, "/") {
				baseImage = options.Base
				break
			}

			// The base is another layer
			source = options.Base
		}
	}

	var base *imageconfig.ImageConfig
	var baseImageManifest *layers.ImageManifest
	var baseImageSpec *DockerImageSpec
	if baseImage != "" {
		baseImageSpec, err = ParseDockerImageSpec(baseImage)
		if err != nil {
			return err
		}
		baseImageManifest, err = layerStore.FindImageManifest(baseImageSpec.Repository, baseImageSpec.Tag)
		if err != nil {
			return err
		}
		if baseImageManifest == nil {
			return fmt.Errorf("base image %q not found", baseImage)
		}

		if baseImageManifest.Config.Digest == "" {
			return fmt.Errorf("base image %q did not have a valid manifest", baseImage)
		}

		configBlob, err := layerStore.FindBlob(baseImageSpec.Repository, baseImageManifest.Config.Digest)
		if err != nil {
			return err
		}

		configBlobReader, err := configBlob.Open()
		if err != nil {
			return err
		}
		defer configBlobReader.Close()

		configBlobBytes, err := ioutil.ReadAll(configBlobReader)
		if err != nil {
			return err
		}

		base = &imageconfig.ImageConfig{}
		err = json.Unmarshal(configBlobBytes, base)
		if err != nil {
			return fmt.Errorf("error parsing config blob %s/%s: %v", baseImageSpec.Repository, baseImageManifest.Config.Digest, err)
		}
	}

	for _, newLayer := range newLayers {
		// BuildTar automatically saves the blob
		blob, diffID, err := newLayer.Layer.BuildTar(layerStore, dest.Repository)
		if err != nil {
			return err
		}
		newLayer.Blob = blob
		newLayer.DiffID = diffID
	}

	config, err := imageconfig.JoinLayer(base, newLayers)
	if err != nil {
		return err
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("error serializing image config: %v", err)
	}

	configDigest := sha256Bytes(configBytes)
	configBlob, err := layerStore.AddBlob(dest.Repository, configDigest, bytes.NewReader(configBytes))
	if err != nil {
		return fmt.Errorf("error storing config blob: %v", err)
	}

	imageManifest := &layers.ImageManifest{}
	imageManifest.Repository = dest.Repository
	imageManifest.Tag = dest.Tag
	imageManifest.Config = layers.LayerManifest{
		Digest: configBlob.Digest(),
		Size:   configBlob.Length(),
	}

	// Add and upload base layers
	if base != nil {
		for i, baseLayer := range baseImageManifest.Layers {
			imageManifest.Layers = append(imageManifest.Layers, layers.LayerManifest{
				Digest: baseLayer.Digest,
				Size:   baseLayer.Size,
			})

			// TODO: Cross-copy blobs ... we don't need to download them
			src, err := layerStore.FindBlob(baseImageSpec.Repository, baseLayer.Digest)
			if err != nil {
				return err
			}
			err = uploadBlob(out, targetRegistry, auth, dest.Repository, src, fmt.Sprintf("%s layer #%d)", baseImageSpec, i+1))
			if err != nil {
				return err
			}
		}
	}

	// Add and upload new layers
	for _, newLayer := range newLayers {
		digest := newLayer.Blob.Digest()

		imageManifest.Layers = append(imageManifest.Layers, layers.LayerManifest{
			Digest: digest,
			Size:   newLayer.Blob.Length(),
		})

		src, err := layerStore.FindBlob(dest.Repository, digest)
		if err != nil {
			return err
		}
		if src == nil {
			return fmt.Errorf("unable to find layer blob %s %s", dest.Repository, digest)
		}
		err = uploadBlob(out, targetRegistry, auth, dest.Repository, src, newLayer.Description)
		if err != nil {
			return err
		}
	}

	// Build and upload the manifest
	{
		err = layerStore.WriteImageManifest(dest.Repository, dest.Tag, imageManifest)
		if err != nil {
			return fmt.Errorf("error writing image manifest: %v", err)
		}

		err = uploadBlob(out, targetRegistry, auth, dest.Repository, configBlob, "image manifest")
		if err != nil {
			return err
		}
	}

	// Push the manifest
	{
		dockerManifest := &docker.ManifestV2{}
		dockerManifest.Config = docker.ManifestV2Layer{
			Digest:    imageManifest.Config.Digest,
			MediaType: "application/vnd.docker.container.image.v1+json",
			Size:      imageManifest.Config.Size,
		}

		dockerManifest.SchemaVersion = 2
		dockerManifest.MediaType = "application/vnd.docker.distribution.manifest.v2+json"

		for _, layer := range imageManifest.Layers {
			dockerManifest.Layers = append(dockerManifest.Layers, docker.ManifestV2Layer{
				Digest:    layer.Digest,
				MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip",
				Size:      layer.Size,
			})
		}
		err := targetRegistry.PutManifest(auth, dest.Repository, dest.Tag, dockerManifest)
		if err != nil {
			return fmt.Errorf("error writing manifest: %v", err)
		}
	}

	fmt.Fprintf(out, "Pushed %s\n", dest)
	return nil
}

func uploadBlob(out io.Writer, registry *docker.Registry, auth *docker.Auth, destRepository string, srcBlob layers.Blob, info string) error {
	digest := srcBlob.Digest()

	hasBlob, err := registry.HasBlob(auth, destRepository, digest)
	if err != nil {
		return err
	}

	if hasBlob {
		glog.V(2).Infof("Already has blob for %s (%s %s)", info, destRepository, digest)
		return nil
	}

	r, err := srcBlob.Open()
	if err != nil {
		return err
	}
	defer r.Close()

	length := srcBlob.Length()
	mb := fmt.Sprintf("%d MB", length/(1024*1024))
	if mb == "0 MB" {
		mb = "< 1MB"
	}
	fmt.Fprintf(out, "Uploading blob: %s (%s, %s)\n", info, mb, digest)
	err = registry.UploadBlob(auth, destRepository, digest, r, srcBlob.Length())
	if err != nil {
		return err
	}

	return nil
}

func dockerDigest(r io.Reader) (string, error) {
	hasher := sha256.New()
	_, err := io.Copy(hasher, r)
	if err != nil {
		return "", fmt.Errorf("error hashing data: %v", err)
	}

	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func sha256Bytes(data []byte) string {
	hasher := sha256.New()
	_, err := hasher.Write(data)
	if err != nil {
		glog.Fatalf("error hashing bytes: %v", err)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}
