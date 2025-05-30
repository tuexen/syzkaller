// Copyright 2025 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"slices"
	"strconv"

	"cloud.google.com/go/civil"
	"github.com/google/syzkaller/pkg/cover"
	"github.com/google/syzkaller/pkg/coveragedb"
	"github.com/google/syzkaller/pkg/coveragedb/spannerclient"
	"github.com/google/syzkaller/pkg/covermerger"
	"github.com/google/syzkaller/pkg/html/urlutil"
	"github.com/google/syzkaller/pkg/validator"
	"google.golang.org/appengine/v2"
)

var coverageDBClient spannerclient.SpannerClient

func initCoverageDB() {
	if !appengine.IsAppEngine() {
		// It is a test environment.
		// Use setCoverageDBClient to specify the coveragedb mock or emulator in every test.
		return
	}
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	var err error
	coverageDBClient, err = spannerclient.NewClient(context.Background(), projectID)
	if err != nil {
		panic("spanner.NewClient: " + err.Error())
	}
}

var keyCoverageDBClient = "coveragedb client key"

func getCoverageDBClient(ctx context.Context) spannerclient.SpannerClient {
	ctxClient, _ := ctx.Value(&keyCoverageDBClient).(spannerclient.SpannerClient)
	if ctxClient == nil && coverageDBClient == nil {
		panic("attempt to get coverage db client before it was set in tests")
	}
	if ctxClient != nil {
		return ctxClient
	}
	return coverageDBClient
}

type funcStyleBodyJS func(
	ctx context.Context, client spannerclient.SpannerClient,
	scope *coveragedb.SelectScope, onlyUnique bool, sss, managers []string, dataFilters cover.Format,
) (template.CSS, template.HTML, template.HTML, error)

func handleCoverageHeatmap(c context.Context, w http.ResponseWriter, r *http.Request) error {
	if r.FormValue("jsonl") == "1" {
		hdr, err := commonHeader(c, r, w, "")
		if err != nil {
			return err
		}
		ns := hdr.Namespace
		repo, _ := getNsConfig(c, ns).mainRepoBranch()
		w.Header().Set("Content-Type", "application/json")
		return writeExtAPICoverageFor(c, w, ns, repo)
	}
	return handleHeatmap(c, w, r, cover.DoHeatMapStyleBodyJS)
}

func handleSubsystemsCoverageHeatmap(c context.Context, w http.ResponseWriter, r *http.Request) error {
	return handleHeatmap(c, w, r, cover.DoSubsystemsHeatMapStyleBodyJS)
}

type covPageParam int

const (
	// keep-sorted start
	CommitHash covPageParam = iota
	DateTo
	FilePath
	ManagerName
	MinCoverLinesDrop
	OrderByCoverDrop
	PeriodCount
	PeriodType
	SubsystemName
	UniqueOnly
	// keep-sorted end
)

var covPageParams = map[covPageParam]string{
	// keep-sorted start
	CommitHash:        "commit",
	DateTo:            "dateto",
	FilePath:          "filepath",
	ManagerName:       "manager",
	MinCoverLinesDrop: "min-cover-lines-drop",
	OrderByCoverDrop:  "order-by-cover-lines-drop",
	PeriodCount:       "period_count",
	PeriodType:        "period",
	SubsystemName:     "subsystem",
	UniqueOnly:        "unique-only",
	// keep-sorted end
}

func coveragePageLink(ns, periodType, dateTo string, minDrop, periodCount int, orderByCoverDrop bool) string {
	if periodType == "" {
		periodType = coveragedb.MonthPeriod
	}
	url := "/" + ns + "/coverage"
	url = urlutil.SetParam(url, covPageParams[PeriodType], periodType)
	if periodCount != 0 {
		url = urlutil.SetParam(url, covPageParams[PeriodCount], strconv.Itoa(periodCount))
	}
	if dateTo != "" {
		url = urlutil.SetParam(url, covPageParams[DateTo], dateTo)
	}
	if minDrop > 0 {
		url = urlutil.SetParam(url, covPageParams[MinCoverLinesDrop], strconv.Itoa(minDrop))
	}
	if orderByCoverDrop {
		url = urlutil.SetParam(url, covPageParams[OrderByCoverDrop], "1")
	}
	return url
}

func handleHeatmap(c context.Context, w http.ResponseWriter, r *http.Request, f funcStyleBodyJS) error {
	hdr, err := commonHeader(c, r, w, "")
	if err != nil {
		return err
	}
	nsConfig := getNsConfig(c, hdr.Namespace)
	if nsConfig.Coverage == nil {
		return ErrClientNotFound
	}
	ss := r.FormValue(covPageParams[SubsystemName])
	manager := r.FormValue(covPageParams[ManagerName])

	periodType := r.FormValue(covPageParams[PeriodType])
	if periodType == "" {
		periodType = coveragedb.DayPeriod
	}
	if periodType != coveragedb.DayPeriod &&
		periodType != coveragedb.MonthPeriod &&
		periodType != coveragedb.QuarterPeriod {
		return fmt.Errorf("only 'day', 'month' and 'quarter' are allowed, but received %s instead, %w",
			periodType, ErrClientBadRequest)
	}

	periodCount := r.FormValue(covPageParams[PeriodCount])
	if periodCount == "" {
		periodCount = "4"
	}
	nPeriods, err := strconv.Atoi(periodCount)
	if err != nil || nPeriods > 12 || nPeriods < 1 {
		return fmt.Errorf("periods_count is wrong, expected [1, 12]: %w", err)
	}

	dateTo := civil.DateOf(timeNow(c))
	if customDate := r.FormValue(covPageParams[DateTo]); customDate != "" {
		if dateTo, err = civil.ParseDate(customDate); err != nil {
			return fmt.Errorf("civil.ParseDate(%s): %w", customDate, err)
		}
	}

	periods, err := coveragedb.GenNPeriodsTill(nPeriods, dateTo, periodType)
	if err != nil {
		return fmt.Errorf("%s: %w", err.Error(), ErrClientBadRequest)
	}
	managers, err := CachedManagerList(c, hdr.Namespace)
	if err != nil {
		return err
	}
	ssService := getNsConfig(c, hdr.Namespace).Subsystems.Service
	var subsystems []string
	for _, ss := range ssService.List() {
		subsystems = append(subsystems, ss.Name)
	}
	slices.Sort(managers)
	slices.Sort(subsystems)

	onlyUnique := r.FormValue(covPageParams[UniqueOnly]) == "1"
	orderByCoverLinesDrop := r.FormValue(covPageParams[OrderByCoverDrop]) == "1"
	// Prefixing "0" we don't fail on empty string.
	minCoverLinesDrop, err := strconv.Atoi("0" + r.FormValue(covPageParams[MinCoverLinesDrop]))
	if err != nil {
		return fmt.Errorf(covPageParams[MinCoverLinesDrop] + " should be integer")
	}

	var style template.CSS
	var body, js template.HTML
	if style, body, js, err = f(c, getCoverageDBClient(c),
		&coveragedb.SelectScope{
			Ns:        hdr.Namespace,
			Subsystem: ss,
			Manager:   manager,
			Periods:   periods,
		},
		onlyUnique, subsystems, managers,
		cover.Format{
			FilterMinCoveredLinesDrop: minCoverLinesDrop,
			OrderByCoveredLinesDrop:   orderByCoverLinesDrop,
			DropCoveredLines0:         onlyUnique,
		}); err != nil {
		return fmt.Errorf("failed to generate heatmap: %w", err)
	}
	return serveTemplate(w, "custom_content.html", struct {
		Header *uiHeader
		*cover.StyleBodyJS
	}{
		Header: hdr,
		StyleBodyJS: &cover.StyleBodyJS{
			Style: style,
			Body:  body,
			JS:    js,
		},
	})
}

func makeProxyURIProvider(url string) covermerger.FuncProxyURI {
	return func(filePath, commit string) string {
		// Parameter format=TEXT is ignored by git servers but is processed by gerrit servers.
		// Gerrit returns base64 encoded data.
		// Git return the plain text data.
		return fmt.Sprintf("%s/%s/%s?format=TEXT", url, commit, filePath)
	}
}

func handleFileCoverage(c context.Context, w http.ResponseWriter, r *http.Request) error {
	hdr, err := commonHeader(c, r, w, "")
	if err != nil {
		return err
	}
	nsConfig := getNsConfig(c, hdr.Namespace)
	if nsConfig.Coverage == nil || nsConfig.Coverage.WebGitURI == "" {
		return ErrClientNotFound
	}
	dateToStr := r.FormValue(covPageParams[DateTo])
	periodType := r.FormValue(covPageParams[PeriodType])
	targetCommit := r.FormValue(covPageParams[CommitHash])
	kernelFilePath := r.FormValue(covPageParams[FilePath])
	manager := r.FormValue(covPageParams[ManagerName])
	if err := validator.AnyError("input validation failed",
		validator.TimePeriodType(periodType, covPageParams[PeriodType]),
		validator.CommitHash(targetCommit, covPageParams[CommitHash]),
		validator.KernelFilePath(kernelFilePath, covPageParams[FilePath]),
		validator.AnyOk(
			validator.Allowlisted(manager, []string{"", "*"}, covPageParams[ManagerName]),
			validator.ManagerName(manager, covPageParams[ManagerName])),
	); err != nil {
		return fmt.Errorf("%w: %w", err, ErrClientBadRequest)
	}
	targetDate, err := civil.ParseDate(dateToStr)
	if err != nil {
		return fmt.Errorf("civil.ParseDate(%s): %w", dateToStr, err)
	}
	tp, err := coveragedb.MakeTimePeriod(targetDate, periodType)
	if err != nil {
		return fmt.Errorf("coveragedb.MakeTimePeriod: %w", err)
	}
	onlyUnique := r.FormValue(covPageParams[UniqueOnly]) == "1"
	mainNsRepo, _ := nsConfig.mainRepoBranch()
	client := getCoverageDBClient(c)
	if client == nil {
		return fmt.Errorf("spannerdb client is nil")
	}
	hitLines, hitCounts, err := coveragedb.ReadLinesHitCount(
		c, client, hdr.Namespace, targetCommit, kernelFilePath, manager, tp)
	covMap := coveragedb.MakeCovMap(hitLines, hitCounts)
	if err != nil {
		return fmt.Errorf("coveragedb.ReadLinesHitCount(%s): %w", manager, err)
	}
	if onlyUnique {
		// This request is expected to be made second by tests.
		// Moving it to goroutine don't forget to change multiManagerCovDBFixture.
		allHitLines, allHitCounts, err := coveragedb.ReadLinesHitCount(
			c, client, hdr.Namespace, targetCommit, kernelFilePath, "*", tp)
		if err != nil {
			return fmt.Errorf("coveragedb.ReadLinesHitCount(*): %w", err)
		}
		covMap = coveragedb.UniqCoverage(coveragedb.MakeCovMap(allHitLines, allHitCounts), covMap)
	}

	webGit := getWebGit(c) // Get mock if available.
	if webGit == nil {
		webGit = covermerger.MakeWebGit(makeProxyURIProvider(nsConfig.Coverage.WebGitURI))
	}

	content, err := cover.RendFileCoverage(
		mainNsRepo,
		targetCommit,
		kernelFilePath,
		webGit,
		&covermerger.MergeResult{HitCounts: covMap},
		cover.DefaultHTMLRenderConfig())
	if err != nil {
		return fmt.Errorf("cover.RendFileCoverage: %w", err)
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(content))
	return nil
}

var keyWebGit = "file content provider"

func setWebGit(ctx context.Context, provider covermerger.FileVersProvider) context.Context {
	return context.WithValue(ctx, &keyWebGit, provider)
}

func getWebGit(ctx context.Context) covermerger.FileVersProvider {
	res, _ := ctx.Value(&keyWebGit).(covermerger.FileVersProvider)
	return res
}

func handleCoverageGraph(c context.Context, w http.ResponseWriter, r *http.Request) error {
	hdr, err := commonHeader(c, r, w, "")
	if err != nil {
		return err
	}
	nsConfig := getNsConfig(c, hdr.Namespace)
	if nsConfig.Coverage == nil {
		return ErrClientNotFound
	}
	periodType := r.FormValue(covPageParams[PeriodType])
	if periodType == "" {
		periodType = coveragedb.QuarterPeriod
	}
	if periodType != coveragedb.QuarterPeriod && periodType != coveragedb.MonthPeriod {
		return fmt.Errorf("only quarter and month are allowed, but received %s instead", periodType)
	}
	hist, err := MergedCoverage(c, getCoverageDBClient(c), hdr.Namespace, periodType)
	if err != nil {
		return err
	}
	periodEndDates, err := coveragedb.GenNPeriodsTill(12, civil.DateOf(timeNow(c)), periodType)
	if err != nil {
		return err
	}
	cols := []uiGraphColumn{}
	for _, periodEndDate := range periodEndDates {
		date := periodEndDate.DateTo.String()
		if _, ok := hist.covered[date]; !ok || hist.instrumented[date] == 0 {
			cols = append(cols, uiGraphColumn{Hint: date, Vals: []uiGraphValue{{IsNull: true}}})
		} else {
			val := float32(hist.covered[date]) / float32(hist.instrumented[date])
			cols = append(cols, uiGraphColumn{
				Hint:       date,
				Annotation: val,
				Vals:       []uiGraphValue{{Val: val}},
			})
		}
	}
	data := &uiHistogramPage{
		Title:  hdr.Namespace + " coverage",
		Header: hdr,
		Graph: &uiGraph{
			Headers: []uiGraphHeader{
				{Name: "Total", Color: "Red"},
			},
			Columns: cols,
		},
	}
	return serveTemplate(w, "graph_histogram.html", data)
}
