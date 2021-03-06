// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"

	"github.com/google/safehtml/template"
	"github.com/google/safehtml/template/uncheckedconversions"
	"golang.org/x/pkgsite/internal"
	"golang.org/x/pkgsite/internal/cookie"
	"golang.org/x/pkgsite/internal/derrors"
	"golang.org/x/pkgsite/internal/experiment"
	"golang.org/x/pkgsite/internal/log"
	"golang.org/x/pkgsite/internal/postgres"
	"golang.org/x/pkgsite/internal/stdlib"
)

// errUnitNotFoundWithoutFetch returns a 404 with instructions to the user on
// how to manually fetch the package. No fetch button is provided. This is used
// for very large modules or modules that previously 500ed.
var errUnitNotFoundWithoutFetch = &serverError{
	status: http.StatusNotFound,
	epage: &errorPage{
		messageTemplate: template.MakeTrustedTemplate(`
					    <h3 class="Error-message">{{.StatusText}}</h3>
					    <p class="Error-message">Check that you entered the URL correctly or try fetching it following the
                        <a href="/about#adding-a-package">instructions here</a>.</p>`),
		MessageData: struct{ StatusText string }{http.StatusText(http.StatusNotFound)},
	},
}

// servePathNotFoundPage serves a 404 page for the requested path, or redirects
// the user to an appropriate location.
func (s *Server) servePathNotFoundPage(w http.ResponseWriter, r *http.Request,
	ds internal.DataSource, fullPath, requestedVersion string) (err error) {
	defer derrors.Wrap(&err, "servePathNotFoundPage(w, r, %q, %q)", fullPath, requestedVersion)

	db, ok := ds.(*postgres.DB)
	if !ok {
		return proxydatasourceNotSupportedErr()
	}
	ctx := r.Context()

	if stdlib.Contains(fullPath) {
		var path string
		path, err = stdlibPathForShortcut(ctx, db, fullPath)
		if err != nil {
			// Log the error, but prefer a "path not found" error for a
			// better user experience.
			log.Error(ctx, err)
		}
		if path != "" {
			http.Redirect(w, r, fmt.Sprintf("/%s", path), http.StatusFound)
			return
		}
		return &serverError{status: http.StatusNotFound}
	}

	fr, err := previousFetchStatusAndResponse(ctx, db, fullPath, requestedVersion)
	if err != nil {
		if err != nil {
			log.Error(ctx, err)
		}
		return pathNotFoundError(fullPath, requestedVersion)
	}
	switch fr.status {
	case http.StatusFound, derrors.ToStatus(derrors.AlternativeModule):
		u := constructUnitURL(fr.goModPath, fr.goModPath, internal.LatestVersion)
		cookie.Set(w, cookie.AlternativeModuleFlash, fullPath, u)
		http.Redirect(w, r, constructUnitURL(fr.goModPath, fr.goModPath, internal.LatestVersion), http.StatusFound)
		return
	case http.StatusInternalServerError:
		return pathNotFoundError(fullPath, requestedVersion)
	default:
		return &serverError{
			status: fr.status,
			epage: &errorPage{
				messageTemplate: uncheckedconversions.TrustedTemplateFromStringKnownToSatisfyTypeContract(`
					    <h3 class="Error-message">{{.StatusText}}</h3>
					    <p class="Error-message">` + html.UnescapeString(fr.responseText) + `</p>`),
				MessageData: struct{ StatusText string }{http.StatusText(fr.status)},
			},
		}
	}
}

// pathNotFoundError returns a page with an option on how to
// add a package or module to the site.
func pathNotFoundError(fullPath, requestedVersion string) error {
	if !isSupportedVersion(fullPath, requestedVersion) {
		return invalidVersionError(fullPath, requestedVersion)
	}
	if stdlib.Contains(fullPath) {
		return &serverError{status: http.StatusNotFound}
	}
	path := fullPath
	if requestedVersion != internal.LatestVersion {
		path = fmt.Sprintf("%s@%s", fullPath, requestedVersion)
	}
	return &serverError{
		status: http.StatusNotFound,
		epage: &errorPage{
			templateName: "fetch.tmpl",
			MessageData:  path,
		},
	}
}

// previousFetchStatusAndResponse returns the fetch result from a
// previous fetch of the fullPath and requestedVersion.
func previousFetchStatusAndResponse(ctx context.Context, db *postgres.DB, fullPath, requestedVersion string) (_ *fetchResult, err error) {
	defer derrors.Wrap(&err, "previousFetchStatusAndResponse(w, r, %q, %q)", fullPath, requestedVersion)

	// Check if a row exists in the version_map table for the requested path
	// and version. If not, this path may have never been fetched.
	// In that case, a derrors.NotFound will be returned.
	vm, err := db.GetVersionMap(ctx, fullPath, requestedVersion)
	if err != nil {
		return nil, err
	}

	// If the row has been fetched before, and the result was either a 490 or
	// 491, return that result, since it is a final state.
	if vm.Status >= 500 ||
		vm.Status == derrors.ToStatus(derrors.AlternativeModule) ||
		vm.Status == derrors.ToStatus(derrors.BadModule) {
		return resultFromFetchRequest([]*fetchResult{
			{
				modulePath: vm.ModulePath,
				goModPath:  vm.GoModPath,
				status:     vm.Status,
				err:        errors.New(vm.Error),
			},
		}, fullPath, requestedVersion)
	}

	if experiment.IsActive(ctx, internal.ExperimentNotAtV1) {
		// Check if the unit path exists at a higher major version.
		// For example, my.module might not exist, but my.module/v3 might.
		// Similarly, my.module/foo might not exist, but my.module/v3/foo might.
		// In either case, the user will be redirected to the highest major version
		// of the path.
		//
		// Do not bother to look for a specific version if this case. If
		// my.module/foo@v2.1.0 was requested, and my.module/foo/v2 exists, just
		// return the latest version of my.module/foo/v2.
		majPath, err := db.GetLatestMajorPathForV1Path(ctx, fullPath)
		if err != nil && err != derrors.NotFound {
			return nil, err
		}
		if majPath != "" {
			return &fetchResult{
				modulePath: majPath,
				goModPath:  majPath,
				status:     http.StatusFound,
			}, nil
		}
	}

	// The full path does not exist in our database, but its module might.
	// This could be be because the path is in an alternative module or a bad
	// module, or it was fetched previously and 404ed.
	paths, err := candidateModulePaths(fullPath)
	if err != nil {
		return nil, err
	}
	vms, err := db.GetVersionMapsNon2xxStatus(ctx, paths, requestedVersion)
	if err != nil {
		return nil, err
	}
	if len(vms) == 0 {
		return nil, nil
	}
	var fetchResults []*fetchResult
	for _, vm := range vms {
		fetchResults = append(fetchResults, fetchResultFromVersionMap(vm))
	}
	if len(fetchResults) == 0 {
		return nil, derrors.NotFound
	}
	return resultFromFetchRequest(fetchResults, fullPath, requestedVersion)
}

func fetchResultFromVersionMap(vm *internal.VersionMap) *fetchResult {
	var err error
	if vm.Error != "" {
		err = errors.New(vm.Error)
	}
	return &fetchResult{
		modulePath: vm.ModulePath,
		goModPath:  vm.GoModPath,
		status:     vm.Status,
		err:        err,
	}
}
