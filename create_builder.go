package pack

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Masterminds/semver"
	"github.com/buildpack/imgutil"
	"github.com/pkg/errors"

	"github.com/buildpack/pack/builder"
	"github.com/buildpack/pack/image"
	"github.com/buildpack/pack/style"
)

type CreateBuilderOptions struct {
	BuilderName   string
	BuilderConfig builder.Config
	Publish       bool
	NoPull        bool
}

// pack create-builder
// create new builder
func (c *Client) CreateBuilder(ctx context.Context, opts CreateBuilderOptions) error {
	if err := validateBuilderConfig(opts.BuilderConfig); err != nil {
		return errors.Wrap(err, "invalid builder config")
	}

	if err := c.validateRunImageConfig(ctx, opts); err != nil {
		return err
	}

	baseImage, err := c.imageFetcher.Fetch(ctx, opts.BuilderConfig.Stack.BuildImage, !opts.Publish, !opts.NoPull)
	if err != nil {
		return err
	}

	runImage, err := c.imageFetcher.Fetch(ctx, opts.BuilderConfig.Stack.RunImage, !opts.Publish, !opts.NoPull)
	if err != nil {
		return errors.Wrapf(err, "fetching run image %s", style.Symbol(opts.BuilderConfig.Stack.RunImage))
	}

	// validate build <-> run (builder.toml) -- strict

	// TODO: replace with GetLabel
	mixinsData, err := baseImage.Label(builder.MixinsLabel)
	if err != nil {
		return err
	}
	var buildMixins []string
	if mixinsData != "" {
		if err := json.Unmarshal([]byte(mixinsData), &buildMixins); err != nil {
			return err
		}
	}

	// TODO: replace with GetLabel
	mixinsData, err = runImage.Label(builder.MixinsLabel)
	if err != nil {
		return err
	}
	var runMixins []string
	if mixinsData != "" {
		if err := json.Unmarshal([]byte(mixinsData), &runMixins); err != nil {
			return err
		}
	}
	mixins := mergeMixins(buildMixins, runMixins)

	c.logger.Debugf("Creating builder %s from build-image %s", style.Symbol(opts.BuilderName), style.Symbol(baseImage.Name()))
	bldr, err := builder.New(baseImage, opts.BuilderName)
	if err != nil {
		return errors.Wrap(err, "invalid build-image")
	}

	bldr.SetDescription(opts.BuilderConfig.Description)

	if bldr.StackID != opts.BuilderConfig.Stack.ID {
		return fmt.Errorf(
			"stack %s from builder config is incompatible with stack %s from build image",
			style.Symbol(opts.BuilderConfig.Stack.ID),
			style.Symbol(bldr.StackID),
		)
	}

	bldr.SetMixins(mixins)

	lifecycle, err := c.fetchLifecycle(ctx, opts.BuilderConfig.Lifecycle)
	if err != nil {
		return errors.Wrap(err, "fetch lifecycle")
	}

	if err := bldr.SetLifecycle(lifecycle); err != nil {
		return errors.Wrap(err, "setting lifecycle")
	}

	for _, b := range opts.BuilderConfig.Buildpacks {
		err := ensureBPSupport(b.URI)
		if err != nil {
			return err
		}

		blob, err := c.downloader.Download(ctx, b.URI)
		if err != nil {
			return errors.Wrapf(err, "downloading buildpack from %s", style.Symbol(b.URI))
		}

		fetchedBp, err := builder.NewBuildpack(blob)
		if err != nil {
			return errors.Wrap(err, "creating buildpack")
		}

		err = validateBuildpack(fetchedBp, b.URI, b.ID, b.Version)
		if err != nil {
			return errors.Wrap(err, "invalid buildpack")
		}

		bldr.AddBuildpack(fetchedBp)
	}

	bldr.SetOrder(opts.BuilderConfig.Order)
	bldr.SetStack(opts.BuilderConfig.Stack)

	return bldr.Save(c.logger)
}

func mergeMixins(buildMixins []string, runMixins []string) []string {
	set := map[string]interface{}{}
	for _, m := range buildMixins {
		set[m] = nil
	}
	for _, m := range runMixins {
		set[m] = nil
	}
	var merged []string
	for m := range set {
		merged = append(merged, m)
	}
	return merged
}

func validateBuildpack(bp builder.Buildpack, source, expectedID, expectedBPVersion string) error {
	if expectedID != "" && bp.Descriptor().Info.ID != expectedID {
		return fmt.Errorf(
			"buildpack from URI %s has ID %s which does not match ID %s from builder config",
			style.Symbol(source),
			style.Symbol(bp.Descriptor().Info.ID),
			style.Symbol(expectedID),
		)
	}

	if expectedBPVersion != "" && bp.Descriptor().Info.Version != expectedBPVersion {
		return fmt.Errorf(
			"buildpack from URI %s has version %s which does not match version %s from builder config",
			style.Symbol(source),
			style.Symbol(bp.Descriptor().Info.Version),
			style.Symbol(expectedBPVersion),
		)
	}

	return nil
}

func (c *Client) fetchLifecycle(ctx context.Context, config builder.LifecycleConfig) (builder.Lifecycle, error) {
	if config.Version != "" && config.URI != "" {
		return nil, errors.Errorf(
			"%s can only declare %s or %s, not both",
			style.Symbol("lifecycle"), style.Symbol("version"), style.Symbol("uri"),
		)
	}

	var uri string
	switch {
	case config.Version != "":
		v, err := semver.NewVersion(config.Version)
		if err != nil {
			return nil, errors.Wrapf(err, "%s must be a valid semver", style.Symbol("lifecycle.version"))
		}

		uri = uriFromLifecycleVersion(*v)
	case config.URI != "":
		uri = config.URI
	default:
		uri = uriFromLifecycleVersion(*semver.MustParse(builder.DefaultLifecycleVersion))
	}

	b, err := c.downloader.Download(ctx, uri)
	if err != nil {
		return nil, errors.Wrap(err, "downloading lifecycle")
	}

	lifecycle, err := builder.NewLifecycle(b)
	if err != nil {
		return nil, errors.Wrap(err, "invalid lifecycle")
	}

	return lifecycle, nil
}

func uriFromLifecycleVersion(version semver.Version) string {
	return fmt.Sprintf("https://github.com/buildpack/lifecycle/releases/download/v%s/lifecycle-v%s+linux.x86-64.tgz", version.String(), version.String())
}

func validateBuilderConfig(conf builder.Config) error {
	if conf.Stack.ID == "" {
		return errors.New("stack.id is required")
	}

	if conf.Stack.BuildImage == "" {
		return errors.New("stack.build-image is required")
	}

	if conf.Stack.RunImage == "" {
		return errors.New("stack.run-image is required")
	}

	return nil
}

func (c *Client) validateRunImageConfig(ctx context.Context, opts CreateBuilderOptions) error {
	var runImages []imgutil.Image
	for _, i := range append([]string{opts.BuilderConfig.Stack.RunImage}, opts.BuilderConfig.Stack.RunImageMirrors...) {
		if !opts.Publish {
			img, err := c.imageFetcher.Fetch(ctx, i, true, false)
			if err != nil {
				if errors.Cause(err) != image.ErrNotFound {
					return err
				}
			} else {
				runImages = append(runImages, img)
				continue
			}
		}

		img, err := c.imageFetcher.Fetch(ctx, i, false, false)
		if err != nil {
			if errors.Cause(err) != image.ErrNotFound {
				return err
			}
			c.logger.Warnf("run image %s is not accessible", style.Symbol(i))
		} else {
			runImages = append(runImages, img)
		}
	}

	for _, img := range runImages {
		stackID, err := img.Label("io.buildpacks.stack.id")
		if err != nil {
			return err
		}

		if stackID != opts.BuilderConfig.Stack.ID {
			return fmt.Errorf(
				"stack %s from builder config is incompatible with stack %s from run image %s",
				style.Symbol(opts.BuilderConfig.Stack.ID),
				style.Symbol(stackID),
				style.Symbol(img.Name()),
			)
		}
	}

	return nil
}
