/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2016 SUSE LLC.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sirupsen/logrus"
	"github.com/cyphar/umoci/image/cas"
	"github.com/cyphar/umoci/image/generator"
	"github.com/cyphar/umoci/image/layerdiff"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli"
	"github.com/vbatts/go-mtree"
	"golang.org/x/net/context"
)

var repackCommand = cli.Command{
	Name:  "repack",
	Usage: "repacks an OCI runtime bundle into a reference",
	ArgsUsage: `--image <image-path> --from <reference> --bundle <bundle-path>

Where "<image-path>" is the path to the OCI image, "<reference>" is the name of
the reference descriptor which was used to generate the original runtime bundle
and "<bundle-path>" is the destination to repack the image to.

It should be noted that this is not the same as oci-create-layer because it
uses go-mtree to create diff layers from runtime bundles unpacked with
umoci-unpack(1). In addition, it modifies the image so that all of the relevant
manifest and configuration information uses the new diff atop the old manifest.`,

	Flags: []cli.Flag{
		// FIXME: This really should be a global option.
		cli.StringFlag{
			Name:  "image",
			Usage: "path to OCI image bundle",
		},
		cli.StringFlag{
			Name:  "from",
			Usage: "reference descriptor name to repack",
		},
		cli.StringFlag{
			Name:  "bundle",
			Usage: "destination bundle path",
		},
		cli.StringFlag{
			Name:  "tag",
			Usage: "tag name for repacked image",
		},
	},

	Action: repack,
}

func repack(ctx *cli.Context) error {
	// FIXME: Is there a nicer way of dealing with mandatory arguments?
	imagePath := ctx.String("image")
	if imagePath == "" {
		return fmt.Errorf("image path cannot be empty")
	}
	bundlePath := ctx.String("bundle")
	if bundlePath == "" {
		return fmt.Errorf("bundle path cannot be empty")
	}
	fromName := ctx.String("from")
	if fromName == "" {
		return fmt.Errorf("reference name cannot be empty")
	}

	// Get a reference to the CAS.
	engine, err := cas.Open(imagePath)
	if err != nil {
		return err
	}
	defer engine.Close()

	fromDescriptor, err := engine.GetReference(context.TODO(), fromName)
	if err != nil {
		return err
	}

	// FIXME: Implement support for manifest lists.
	if fromDescriptor.MediaType != v1.MediaTypeImageManifest {
		return fmt.Errorf("--from descriptor does not point to v1.MediaTypeImageManifest: not implemented: %s", fromDescriptor.MediaType)
	}

	// FIXME: We should probably fix this so we don't use ':' in a pathname.
	mtreePath := filepath.Join(bundlePath, fromDescriptor.Digest+".mtree")
	fullRootfsPath := filepath.Join(bundlePath, rootfsName)

	logrus.WithFields(logrus.Fields{
		"image":  imagePath,
		"bundle": bundlePath,
		"ref":    fromName,
		"rootfs": rootfsName,
		"mtree":  mtreePath,
	}).Debugf("umoci: repacking OCI image")

	mfh, err := os.Open(mtreePath)
	if err != nil {
		return err
	}
	defer mfh.Close()

	spec, err := mtree.ParseSpec(mfh)
	if err != nil {
		return err
	}

	keywords := mtree.CollectUsedKeywords(spec)

	diffs, err := mtree.Check(fullRootfsPath, spec, keywords)
	if err != nil {
		return err
	}

	reader, err := layerdiff.GenerateLayer(fullRootfsPath, diffs)
	if err != nil {
		return err
	}
	defer reader.Close()

	// XXX: I get the feeling all of this should be moved to a separate package
	//      which abstracts this nicely.

	layerDigest, layerSize, err := engine.PutBlob(context.TODO(), reader)
	if err != nil {
		return err
	}
	reader.Close()
	// XXX: Should we defer a DeleteBlob?

	layerDescriptor := &v1.Descriptor{
		// FIXME: This should probably be configurable, so someone can specify
		//        that a layer is not distributable.
		MediaType: v1.MediaTypeImageLayer,
		Digest:    layerDigest,
		Size:      layerSize,
	}

	manifestBlob, err := cas.FromDescriptor(context.TODO(), engine, fromDescriptor)
	if err != nil {
		return err
	}
	defer manifestBlob.Close()

	manifest, ok := manifestBlob.Data.(*v1.Manifest)
	if !ok {
		// Should never be reached.
		return fmt.Errorf("manifest blob type not implemented: %s", manifestBlob.MediaType)
	}

	// We also need to update the config. Fun.
	configBlob, err := cas.FromDescriptor(context.TODO(), engine, &manifest.Config)
	if err != nil {
		return err
	}
	defer configBlob.Close()

	config, ok := configBlob.Data.(*v1.Image)
	if !ok {
		// Should not be reached.
		return fmt.Errorf("config blob type not implemented: %s", configBlob.MediaType)
	}

	g, err := generator.NewFromImage(*config)
	if err != nil {
		return err
	}

	// Append our new layer to the set of DiffIDs.
	g.AddRootfsDiffID(layerDigest)

	// Update config and create a new blob for it.
	*config = g.Image()
	newConfigDigest, newConfigSize, err := engine.PutBlobJSON(context.TODO(), config)
	if err != nil {
		return err
	}

	// Update the manifest to include the new layer, and also point at the new
	// config. Then create a new blob for it.
	manifest.Layers = append(manifest.Layers, *layerDescriptor)
	manifest.Config.Digest = newConfigDigest
	manifest.Config.Size = newConfigSize
	newManifestDigest, newManifestSize, err := engine.PutBlobJSON(context.TODO(), manifest)

	// Now create a new reference, and either add it to the engine or spew it
	// to stdout.

	newDescriptor := &v1.Descriptor{
		// FIXME: Support manifest lists.
		MediaType: v1.MediaTypeImageManifest,
		Digest:    newManifestDigest,
		Size:      newManifestSize,
	}

	logrus.WithFields(logrus.Fields{
		"mediatype": newDescriptor.MediaType,
		"digest":    newDescriptor.Digest,
		"size":      newDescriptor.Size,
	}).Infof("created new image")

	tagName := ctx.String("tag")
	if tagName == "" {
		return nil
	}

	// We have to clobber the old reference.
	// XXX: Should we output some warning if we actually did remove an old
	//      reference?
	if err := engine.DeleteReference(context.TODO(), tagName); err != nil {
		return err
	}
	if err := engine.PutReference(context.TODO(), tagName, newDescriptor); err != nil {
		return err
	}

	return nil
}