/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2016, 2017, 2018 SUSE LLC.
 * Copyright (C) 2018 Cisco
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
	"context"
	"time"

	"github.com/apex/log"
	"github.com/openSUSE/umoci/mutate"
	"github.com/openSUSE/umoci/oci/cas/dir"
	"github.com/openSUSE/umoci/oci/casext"
	igen "github.com/openSUSE/umoci/oci/config/generate"
	"github.com/openSUSE/umoci/oci/layer"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var insertCommand = uxRemap(uxHistory(cli.Command{
	Name:  "insert",
	Usage: "insert a file into an OCI image without unpacking/repacking it",
	ArgsUsage: `--image <image-path>[:<tag>] <file> <path>

Where "<image-path>" is the path to the OCI image, "<tag>" is the name of the
tag that the content wil be inserted into (if not specified, defaults to
"latest"), "<file>" is the file or folder to insert, and "<path>" is the full
name of the path to that the file should be inserted at. Insert is
automatically recursive if the source is a directory.

For example:
	umoci insert --image oci:foo mybinary /usr/bin/mybinary
	umoci insert --image oci:foo myconfigdir /etc/myconfigdir
`,

	Category: "image",

	Action: insert,

	Before: func(ctx *cli.Context) error {
		if ctx.NArg() != 2 {
			return errors.Errorf("invalid number of positional arguments: expected <file> and <path>")
		}
		if ctx.Args()[0] == "" {
			return errors.Errorf("<file> cannot be empty")
		}
		ctx.App.Metadata["insertFile"] = ctx.Args()[0]

		if ctx.Args()[1] == "" {
			return errors.Errorf("<path> cannot be empty")
		}
		ctx.App.Metadata["insertPath"] = ctx.Args()[1]

		return nil
	},
}))

func insert(ctx *cli.Context) error {
	imagePath := ctx.App.Metadata["--image-path"].(string)
	tagName := ctx.App.Metadata["--image-tag"].(string)

	// Get a reference to the CAS.
	engine, err := dir.Open(imagePath)
	if err != nil {
		return errors.Wrap(err, "open CAS")
	}
	engineExt := casext.NewEngine(engine)
	defer engine.Close()

	descriptorPaths, err := engineExt.ResolveReference(context.Background(), tagName)
	if err != nil {
		return errors.Wrap(err, "get descriptor")
	}
	if len(descriptorPaths) == 0 {
		return errors.Errorf("tag not found: %s", tagName)
	}
	if len(descriptorPaths) != 1 {
		// TODO: Handle this more nicely.
		return errors.Errorf("tag is ambiguous: %s", tagName)
	}

	// Create the mutator.
	mutator, err := mutate.New(engine, descriptorPaths[0])
	if err != nil {
		return errors.Wrap(err, "create mutator for base image")
	}

	insertFile := ctx.App.Metadata["insertFile"].(string)
	insertPath := ctx.App.Metadata["insertPath"].(string)

	var meta UmociMeta
	meta.Version = UmociMetaVersion

	// Parse and set up the mapping options.
	err = parseIdmapOptions(&meta, ctx)
	if err != nil {
		return err
	}

	reader := layer.GenerateInsertLayer(insertFile, insertPath, &meta.MapOptions)
	defer reader.Close()

	created := time.Now()
	history := ispec.History{
		Comment:    "",
		Created:    &created,
		CreatedBy:  "umoci insert", // XXX: Should we append argv to this?
		EmptyLayer: false,
	}

	if val, ok := ctx.App.Metadata["--history.author"]; ok {
		history.Author = val.(string)
	}
	if val, ok := ctx.App.Metadata["--history.comment"]; ok {
		history.Comment = val.(string)
	}
	if val, ok := ctx.App.Metadata["--history.created"]; ok {
		created, err := time.Parse(igen.ISO8601, val.(string))
		if err != nil {
			return errors.Wrap(err, "parsing --history.created")
		}
		history.Created = &created
	}
	if val, ok := ctx.App.Metadata["--history.created_by"]; ok {
		history.CreatedBy = val.(string)
	}

	// TODO: We should add a flag to allow for a new layer to be made
	//       non-distributable.
	if err := mutator.Add(context.Background(), reader, history); err != nil {
		return errors.Wrap(err, "add diff layer")
	}

	newDescriptorPath, err := mutator.Commit(context.Background())
	if err != nil {
		return errors.Wrap(err, "commit mutated image")
	}

	log.Infof("new image manifest created: %s->%s", newDescriptorPath.Root().Digest, newDescriptorPath.Descriptor().Digest)

	if err := engineExt.UpdateReference(context.Background(), tagName, newDescriptorPath.Root()); err != nil {
		return errors.Wrap(err, "add new tag")
	}
	log.Infof("updated tag for image manifest: %s", tagName)
	return nil
}
