// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package imagemetadata

import (
	"sort"
	"strings"

	"github.com/juju/loggo"
	"github.com/juju/utils/series"

	"github.com/juju/errors"
	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/environs"
	envmetadata "github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/cloudimagemetadata"
)

var logger = loggo.GetLogger("juju.apiserver.imagemetadata")

func init() {
	common.RegisterStandardFacade("ImageMetadata", 1, NewAPI)
}

// API is the concrete implementation of the api end point
// for loud image metadata manipulations.
type API struct {
	metadata   metadataAcess
	authorizer common.Authorizer
}

// createAPI returns a new image metadata API facade.
func createAPI(
	st metadataAcess,
	resources *common.Resources,
	authorizer common.Authorizer,
) (*API, error) {
	if !authorizer.AuthClient() && !authorizer.AuthEnvironManager() {
		return nil, common.ErrPerm
	}

	return &API{
		metadata:   st,
		authorizer: authorizer,
	}, nil
}

// NewAPI returns a new cloud image metadata API facade.
func NewAPI(
	st *state.State,
	resources *common.Resources,
	authorizer common.Authorizer,
) (*API, error) {
	return createAPI(getState(st), resources, authorizer)
}

// List returns all found cloud image metadata that satisfy
// given filter.
// Returned list contains metadata for custom images first, then public.
func (api *API) List(filter params.ImageMetadataFilter) (params.ListCloudImageMetadataResult, error) {
	found, err := api.metadata.FindMetadata(cloudimagemetadata.MetadataFilter{
		Region:          filter.Region,
		Series:          filter.Series,
		Arches:          filter.Arches,
		Stream:          filter.Stream,
		VirtType:        filter.VirtType,
		RootStorageType: filter.RootStorageType,
	})
	if err != nil {
		return params.ListCloudImageMetadataResult{}, common.ServerError(err)
	}

	var all []params.CloudImageMetadata
	addAll := func(ms []cloudimagemetadata.Metadata) {
		for _, m := range ms {
			all = append(all, parseMetadataToParams(m))
		}
	}

	// Sort source keys in alphabetic order.
	sources := make([]string, len(found))
	i := 0
	for source, _ := range found {
		sources[i] = source
		i++
	}
	sort.Strings(sources)

	for _, source := range sources {
		addAll(found[source])
	}

	return params.ListCloudImageMetadataResult{Result: all}, nil
}

// Save stores given cloud image metadata.
// It supports bulk calls.
func (api *API) Save(metadata params.MetadataSaveParams) (params.ErrorResults, error) {
	all := make([]params.ErrorResult, len(metadata.Metadata))
	for i, one := range metadata.Metadata {
		err := api.metadata.SaveMetadata(parseMetadataFromParams(one))
		all[i] = params.ErrorResult{Error: common.ServerError(err)}
	}
	return params.ErrorResults{Results: all}, nil
}

func parseMetadataToParams(p cloudimagemetadata.Metadata) params.CloudImageMetadata {
	result := params.CloudImageMetadata{
		ImageId:         p.ImageId,
		Stream:          p.Stream,
		Region:          p.Region,
		Series:          p.Series,
		Arch:            p.Arch,
		VirtType:        p.VirtType,
		RootStorageType: p.RootStorageType,
		RootStorageSize: p.RootStorageSize,
		Source:          p.Source,
	}
	return result
}

func parseMetadataFromParams(p params.CloudImageMetadata) cloudimagemetadata.Metadata {
	result := cloudimagemetadata.Metadata{
		cloudimagemetadata.MetadataAttributes{
			Stream:          p.Stream,
			Region:          p.Region,
			Series:          p.Series,
			Arch:            p.Arch,
			VirtType:        p.VirtType,
			RootStorageType: p.RootStorageType,
			RootStorageSize: p.RootStorageSize,
			Source:          p.Source,
		},
		p.ImageId,
	}
	if p.Stream == "" {
		result.Stream = "released"
	}
	if p.Source == "" {
		result.Source = "custom"
	}
	return result
}

// UpdateFromPublishedImages retrieves currently published image metadata and
// updates stored ones accordingly.
func (api *API) UpdateFromPublishedImages() error {
	return api.retrievePublished()
}

func (api *API) retrievePublished() error {
	envCfg, err := api.metadata.EnvironConfig()
	env, err := environs.New(envCfg)
	if err != nil {
		return errors.Annotatef(err, "getting environ")
	}

	sources, err := environs.ImageMetadataSources(env)
	if err != nil {
		return errors.Annotatef(err, "getting environment image metadata sources")
	}

	cons := envmetadata.NewImageConstraint(simplestreams.LookupParams{})
	if inst, ok := env.(simplestreams.HasRegion); !ok {
		return errors.Errorf("environment cloud specification cannot be determined")
	} else {
		// If we can determine current region,
		// we want only metadata specific to this region.
		cloud, err := inst.Region()
		if err != nil {
			return errors.Annotatef(err, "getting provider region information (cloud spec)")
		}
		cons.CloudSpec = cloud
	}

	// We want all relevant metadata from all data sources.
	for _, source := range sources {
		logger.Debugf("looking in data source %v", source.Description())
		metadata, info, err := envmetadata.Fetch([]simplestreams.DataSource{source}, cons, false)
		if err != nil {
			// Do not stop looking in other data sources if there is an issue here.
			logger.Errorf("encountered %v while getting published images metadata from %v", err, source.Description())
			continue
		}
		err = api.saveAll(info, metadata)
		if err != nil {
			// Do not stop looking in other data sources if there is an issue here.
			logger.Errorf("encountered %v while saving published images metadata from %v", err, source.Description())
		}
	}
	return nil
}

func (api *API) saveAll(info *simplestreams.ResolveInfo, published []*envmetadata.ImageMetadata) error {
	// Store converted metadata.
	// Note that whether the metadata actually needs
	// to be stored will be determined within this call.
	errs, err := api.Save(convertToParams(info, published))
	if err != nil {
		return errors.Annotatef(err, "saving published images metadata")
	}
	return processErrors(errs.Results)
}

// convertToParams converts environment-specific images metadata to structured metadata format.
var convertToParams = func(info *simplestreams.ResolveInfo, published []*envmetadata.ImageMetadata) params.MetadataSaveParams {
	metadata := make([]params.CloudImageMetadata, len(published))
	for i, p := range published {
		metadata[i] = params.CloudImageMetadata{
			Source:          info.Source,
			ImageId:         p.Id,
			Stream:          p.Stream,
			Region:          p.RegionName,
			Arch:            p.Arch,
			VirtType:        p.VirtType,
			RootStorageType: p.Storage,
		}
		// Translate version (eg.14.04) to a series (eg. "trusty")
		s, err := series.VersionSeries(p.Version)
		if err != nil {
			logger.Warningf("could not determine series for image id %s: %v", p.Id, err)
			continue
		}
		metadata[i].Series = s
	}

	return params.MetadataSaveParams{Metadata: metadata}
}

func processErrors(errs []params.ErrorResult) error {
	msgs := []string{}
	for _, e := range errs {
		if e.Error != nil && e.Error.Message != "" {
			msgs = append(msgs, e.Error.Message)
		}
	}
	if len(msgs) != 0 {
		return errors.Errorf("saving some image metadata:\n%v", strings.Join(msgs, "\n"))
	}
	return nil
}
