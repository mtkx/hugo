// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"runtime"

	_errors "github.com/pkg/errors"

	"golang.org/x/sync/errgroup"
)

type siteContentProcessor struct {
	site *Site

	handleContent contentHandler

	ctx context.Context

	// The input file bundles.
	fileBundlesChan chan *bundleDir

	// The input file singles.
	fileSinglesChan chan *fileInfo

	// These assets should be just copied to destination.
	fileAssetsChan chan pathLangFile

	numWorkers int

	// The output Pages
	pagesChan chan *pageState

	// Used for partial rebuilds (aka. live reload)
	// Will signal replacement of pages in the site collection.
	partialBuild bool
}

func (s *siteContentProcessor) processBundle(b *bundleDir) {
	select {
	case s.fileBundlesChan <- b:
	case <-s.ctx.Done():
	}
}

func (s *siteContentProcessor) processSingle(fi *fileInfo) {
	select {
	case s.fileSinglesChan <- fi:
	case <-s.ctx.Done():
	}
}

func (s *siteContentProcessor) processAsset(asset pathLangFile) {
	select {
	case s.fileAssetsChan <- asset:
	case <-s.ctx.Done():
	}
}

func newSiteContentProcessor(ctx context.Context, partialBuild bool, s *Site) *siteContentProcessor {
	numWorkers := 12
	if n := runtime.NumCPU() * 3; n > numWorkers {
		numWorkers = n
	}

	numWorkers = int(math.Ceil(float64(numWorkers) / float64(len(s.h.Sites))))

	return &siteContentProcessor{
		ctx:             ctx,
		partialBuild:    partialBuild,
		site:            s,
		handleContent:   newHandlerChain(s),
		fileBundlesChan: make(chan *bundleDir, numWorkers),
		fileSinglesChan: make(chan *fileInfo, numWorkers),
		fileAssetsChan:  make(chan pathLangFile, numWorkers),
		numWorkers:      numWorkers,
		pagesChan:       make(chan *pageState, numWorkers),
	}
}

func (s *siteContentProcessor) closeInput() {
	close(s.fileSinglesChan)
	close(s.fileBundlesChan)
	close(s.fileAssetsChan)
}

func (s *siteContentProcessor) process(ctx context.Context) error {
	g1, ctx := errgroup.WithContext(ctx)
	g2, ctx := errgroup.WithContext(ctx)

	// There can be only one of these per site.
	g1.Go(func() error {
		for p := range s.pagesChan {
			if p.s != s.site {
				panic(fmt.Sprintf("invalid page site: %v vs %v", p.s, s))
			}

			p.forceRender = s.partialBuild

			if p.forceRender {
				s.site.replacePage(p)
			} else {
				s.site.addPage(p)
			}
		}
		return nil
	})

	for i := 0; i < s.numWorkers; i++ {
		g2.Go(func() error {
			for {
				select {
				case f, ok := <-s.fileSinglesChan:
					if !ok {
						return nil
					}

					err := s.readAndConvertContentFile(f)
					if err != nil {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})

		g2.Go(func() error {
			for {
				select {
				case file, ok := <-s.fileAssetsChan:
					if !ok {
						return nil
					}
					f, err := s.site.BaseFs.Content.Fs.Open(file.Filename())
					if err != nil {
						return _errors.Wrap(err, "failed to open assets file")
					}
					filename := filepath.Join(s.site.GetTargetLanguageBasePath(), file.Path())
					err = s.site.publish(&s.site.PathSpec.ProcessingStats.Files, filename, f)
					f.Close()
					if err != nil {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})

		g2.Go(func() error {
			for {
				select {
				case bundle, ok := <-s.fileBundlesChan:
					if !ok {
						return nil
					}
					err := s.readAndConvertContentBundle(bundle)
					if err != nil {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
	}

	err := g2.Wait()

	close(s.pagesChan)

	if err != nil {
		return err
	}

	if err := g1.Wait(); err != nil {
		return err
	}

	return nil

}

func (s *siteContentProcessor) readAndConvertContentFile(file *fileInfo) error {
	ctx := &handlerContext{source: file, pages: s.pagesChan}
	return s.handleContent(ctx).err
}

func (s *siteContentProcessor) readAndConvertContentBundle(bundle *bundleDir) error {
	ctx := &handlerContext{bundle: bundle, pages: s.pagesChan}
	return s.handleContent(ctx).err
}
