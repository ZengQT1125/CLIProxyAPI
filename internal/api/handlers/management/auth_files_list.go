package management

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type authFileSort string

const (
	authFileSortDefault  authFileSort = "default"
	authFileSortAZ       authFileSort = "az"
	authFileSortPriority authFileSort = "priority"
)

type authFileListQuery struct {
	Page         int
	PageSize     int
	Type         string
	ProblemOnly  bool
	DisabledOnly bool
	EnabledOnly  bool
	Search       string
	Sort         authFileSort
}

type authFileListPage struct {
	Auths             []*coreauth.Auth
	Total             int
	Page              int
	PageSize          int
	Types             []string
	TypeCounts        map[string]int
	EnabledTypeCounts map[string]int
}

func parseAuthFileListQuery(c *gin.Context) (authFileListQuery, bool, error) {
	query := authFileListQuery{
		Page:     1,
		PageSize: 12,
		Sort:     authFileSortDefault,
	}
	if c == nil {
		return query, false, nil
	}

	_, hasPage := c.GetQuery("page")
	_, hasPageSize := c.GetQuery("page_size")
	if hasPage {
		page, errPage := strconv.Atoi(strings.TrimSpace(c.Query("page")))
		if errPage != nil || page < 1 {
			return query, false, fmt.Errorf("page must be an integer greater than or equal to 1")
		}
		query.Page = page
	}
	if hasPageSize {
		pageSize, errPageSize := strconv.Atoi(strings.TrimSpace(c.Query("page_size")))
		if errPageSize != nil || pageSize < 3 || pageSize > 40 {
			return query, false, fmt.Errorf("page_size must be an integer between 3 and 40")
		}
		query.PageSize = pageSize
	}

	query.Type = normalizeAuthFileType(c.Query("type"))
	query.Search = strings.TrimSpace(c.Query("search"))

	switch authFileSort(strings.ToLower(strings.TrimSpace(c.Query("sort")))) {
	case "", authFileSortDefault:
		query.Sort = authFileSortDefault
	case authFileSortAZ:
		query.Sort = authFileSortAZ
	case authFileSortPriority:
		query.Sort = authFileSortPriority
	default:
		return query, false, fmt.Errorf("sort must be one of default, az, priority")
	}

	var errBool error
	if query.ProblemOnly, errBool = parseAuthFileListBool(c, "problem_only"); errBool != nil {
		return query, false, errBool
	}
	if query.DisabledOnly, errBool = parseAuthFileListBool(c, "disabled_only"); errBool != nil {
		return query, false, errBool
	}
	if query.EnabledOnly, errBool = parseAuthFileListBool(c, "enabled_only"); errBool != nil {
		return query, false, errBool
	}

	return query, hasPage || hasPageSize, nil
}

func parseAuthFileListBool(c *gin.Context, key string) (bool, error) {
	raw, present := c.GetQuery(key)
	if !present {
		return false, nil
	}
	value, errParse := strconv.ParseBool(strings.TrimSpace(raw))
	if errParse != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

func buildAuthFileListPage(auths []*coreauth.Auth, query authFileListQuery) authFileListPage {
	query = normalizeAuthFileListQuery(query)
	page := authFileListPage{
		Page:              query.Page,
		PageSize:          query.PageSize,
		TypeCounts:        map[string]int{"all": 0},
		EnabledTypeCounts: map[string]int{"all": 0},
	}

	types := map[string]struct{}{}
	for _, auth := range auths {
		if !authFileListVisible(auth) {
			continue
		}
		typeName := normalizeAuthFileType(auth.Provider)
		if typeName != "" {
			types[typeName] = struct{}{}
		}
		if !authFileIsDisabled(auth) {
			page.EnabledTypeCounts["all"]++
			if typeName != "" {
				page.EnabledTypeCounts[typeName]++
			}
		}
	}
	page.Types = sortedAuthFileTypes(types)

	filtered := make([]*coreauth.Auth, 0, len(auths))
	statusQuery := query
	statusQuery.Type = ""
	for _, auth := range auths {
		if !authFileListVisible(auth) || !authMatchesListStatusFilters(auth, statusQuery) {
			continue
		}
		typeName := normalizeAuthFileType(auth.Provider)
		page.TypeCounts["all"]++
		if typeName != "" {
			page.TypeCounts[typeName]++
		}
		if !authMatchesListStatusFilters(auth, query) {
			continue
		}
		if query.Search != "" && !authMatchesAuthFileSearch(auth, query.Search) {
			continue
		}
		filtered = append(filtered, auth)
	}

	page.Total = len(filtered)
	sortAuthFileList(filtered, query.Sort)
	start := (query.Page - 1) * query.PageSize
	if start >= len(filtered) {
		page.Auths = []*coreauth.Auth{}
		return page
	}
	end := start + query.PageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	page.Auths = filtered[start:end]
	return page
}

func normalizeAuthFileListQuery(query authFileListQuery) authFileListQuery {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 12
	}
	query.Type = normalizeAuthFileType(query.Type)
	query.Search = strings.TrimSpace(query.Search)
	if query.Sort == "" {
		query.Sort = authFileSortDefault
	}
	return query
}

func authMatchesListStatusFilters(auth *coreauth.Auth, query authFileListQuery) bool {
	if auth == nil {
		return false
	}
	if query.Type != "" && normalizeAuthFileType(auth.Provider) != normalizeAuthFileType(query.Type) {
		return false
	}
	if query.ProblemOnly && strings.TrimSpace(auth.StatusMessage) == "" {
		return false
	}
	disabled := authFileIsDisabled(auth)
	if query.DisabledOnly && !disabled {
		return false
	}
	if query.EnabledOnly && disabled {
		return false
	}
	return true
}

func authFileListVisible(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && authFileIsDisabled(auth) {
		return false
	}
	return runtimeOnly || strings.TrimSpace(authAttribute(auth, "path")) != ""
}

func authFileIsDisabled(auth *coreauth.Auth) bool {
	return auth != nil && (auth.Disabled || auth.Status == coreauth.StatusDisabled)
}

func authFileWildcardMatch(candidate, pattern string) bool {
	candidate = strings.ToLower(candidate)
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return true
	}
	position := 0
	for _, segment := range strings.Split(pattern, "*") {
		if segment == "" {
			continue
		}
		next := strings.Index(candidate[position:], segment)
		if next < 0 {
			return false
		}
		position += next + len(segment)
	}
	return true
}

func authMatchesAuthFileSearch(auth *coreauth.Auth, search string) bool {
	if auth == nil {
		return false
	}
	return authFileWildcardMatch(authFileListName(auth), search) ||
		authFileWildcardMatch(normalizeAuthFileType(auth.Provider), search) ||
		authFileWildcardMatch(strings.TrimSpace(auth.Provider), search)
}

func authFileListName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	return strings.TrimSpace(auth.ID)
}

func normalizeAuthFileType(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func sortedAuthFileTypes(types map[string]struct{}) []string {
	result := make([]string, 0, len(types)+1)
	result = append(result, "all")
	for typeName := range types {
		result = append(result, typeName)
	}
	sort.Strings(result[1:])
	return result
}

func sortAuthFileList(auths []*coreauth.Auth, order authFileSort) {
	sort.Slice(auths, func(i, j int) bool {
		nameI := strings.ToLower(authFileListName(auths[i]))
		nameJ := strings.ToLower(authFileListName(auths[j]))
		if order != authFileSortAZ {
			priorityI, _ := authFilePriority(auths[i])
			priorityJ, _ := authFilePriority(auths[j])
			if priorityI != priorityJ {
				return priorityI > priorityJ
			}
		}
		return nameI < nameJ
	})
}

func authFilePriority(auth *coreauth.Auth) (int, bool) {
	if auth == nil {
		return 0, false
	}
	if raw := strings.TrimSpace(authAttribute(auth, "priority")); raw != "" {
		if priority, errParse := strconv.Atoi(raw); errParse == nil {
			return priority, true
		}
	}
	if auth.Metadata == nil {
		return 0, false
	}
	switch value := auth.Metadata["priority"].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case string:
		if priority, errParse := strconv.Atoi(strings.TrimSpace(value)); errParse == nil {
			return priority, true
		}
	}
	return 0, false
}

func (h *Handler) writeFullAuthFileList(c *gin.Context, auths []*coreauth.Auth) {
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if entry := h.buildAuthFileEntry(auth); entry != nil {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(http.StatusOK, gin.H{"files": files})
}
