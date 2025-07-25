// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"fmt"
	"net/http"
	"strings"

	activities_model "code.gitea.io/gitea/models/activities"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/templates"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/context"
	issue_service "code.gitea.io/gitea/services/issue"
	pull_service "code.gitea.io/gitea/services/pull"
)

const (
	tplNotification              templates.TplName = "user/notification/notification"
	tplNotificationDiv           templates.TplName = "user/notification/notification_div"
	tplNotificationSubscriptions templates.TplName = "user/notification/notification_subscriptions"
)

// Notifications is the notification list page
func Notifications(ctx *context.Context) {
	prepareUserNotificationsData(ctx)
	if ctx.Written() {
		return
	}
	if ctx.FormBool("div-only") {
		ctx.HTML(http.StatusOK, tplNotificationDiv)
		return
	}
	ctx.HTML(http.StatusOK, tplNotification)
}

func prepareUserNotificationsData(ctx *context.Context) {
	pageType := ctx.FormString("type", ctx.FormString("q")) // "q" is the legacy query parameter for "page type"
	page := max(1, ctx.FormInt("page"))
	perPage := util.IfZero(ctx.FormInt("perPage"), 20) // this value is never used or exposed ....
	queryStatus := util.Iif(pageType == "read", activities_model.NotificationStatusRead, activities_model.NotificationStatusUnread)

	total, err := db.Count[activities_model.Notification](ctx, activities_model.FindNotificationOptions{
		UserID: ctx.Doer.ID,
		Status: []activities_model.NotificationStatus{queryStatus},
	})
	if err != nil {
		ctx.ServerError("ErrGetNotificationCount", err)
		return
	}

	pager := context.NewPagination(int(total), perPage, page, 5)
	if pager.Paginater.Current() < page {
		// use the last page if the requested page is more than total pages
		page = pager.Paginater.Current()
		pager = context.NewPagination(int(total), perPage, page, 5)
	}

	statuses := []activities_model.NotificationStatus{queryStatus, activities_model.NotificationStatusPinned}
	nls, err := db.Find[activities_model.Notification](ctx, activities_model.FindNotificationOptions{
		ListOptions: db.ListOptions{
			PageSize: perPage,
			Page:     page,
		},
		UserID: ctx.Doer.ID,
		Status: statuses,
	})
	if err != nil {
		ctx.ServerError("db.Find[activities_model.Notification]", err)
		return
	}

	notifications := activities_model.NotificationList(nls)

	failCount := 0

	repos, failures, err := notifications.LoadRepos(ctx)
	if err != nil {
		ctx.ServerError("LoadRepos", err)
		return
	}
	notifications = notifications.Without(failures)
	if err := repos.LoadAttributes(ctx); err != nil {
		ctx.ServerError("LoadAttributes", err)
		return
	}
	failCount += len(failures)

	failures, err = notifications.LoadIssues(ctx)
	if err != nil {
		ctx.ServerError("LoadIssues", err)
		return
	}

	if err = notifications.LoadIssuePullRequests(ctx); err != nil {
		ctx.ServerError("LoadIssuePullRequests", err)
		return
	}

	notifications = notifications.Without(failures)
	failCount += len(failures)

	failures, err = notifications.LoadComments(ctx)
	if err != nil {
		ctx.ServerError("LoadComments", err)
		return
	}
	notifications = notifications.Without(failures)
	failCount += len(failures)

	if failCount > 0 {
		ctx.Flash.Error(fmt.Sprintf("ERROR: %d notifications were removed due to missing parts - check the logs", failCount))
	}

	ctx.Data["Title"] = ctx.Tr("notifications")
	ctx.Data["PageType"] = pageType
	ctx.Data["Notifications"] = notifications
	ctx.Data["Link"] = setting.AppSubURL + "/notifications"
	ctx.Data["SequenceNumber"] = ctx.FormString("sequence-number")
	pager.AddParamFromRequest(ctx.Req)
	ctx.Data["Page"] = pager
}

// NotificationStatusPost is a route for changing the status of a notification
func NotificationStatusPost(ctx *context.Context) {
	notificationID := ctx.FormInt64("notification_id")
	var newStatus activities_model.NotificationStatus
	switch ctx.FormString("notification_action") {
	case "mark_as_read":
		newStatus = activities_model.NotificationStatusRead
	case "mark_as_unread":
		newStatus = activities_model.NotificationStatusUnread
	case "pin":
		newStatus = activities_model.NotificationStatusPinned
	default:
		return // ignore user's invalid input
	}
	if _, err := activities_model.SetNotificationStatus(ctx, notificationID, ctx.Doer, newStatus); err != nil {
		ctx.ServerError("SetNotificationStatus", err)
		return
	}

	prepareUserNotificationsData(ctx)
	if ctx.Written() {
		return
	}
	ctx.HTML(http.StatusOK, tplNotificationDiv)
}

// NotificationPurgePost is a route for 'purging' the list of notifications - marking all unread as read
func NotificationPurgePost(ctx *context.Context) {
	err := activities_model.UpdateNotificationStatuses(ctx, ctx.Doer, activities_model.NotificationStatusUnread, activities_model.NotificationStatusRead)
	if err != nil {
		ctx.ServerError("UpdateNotificationStatuses", err)
		return
	}

	ctx.Redirect(setting.AppSubURL+"/notifications", http.StatusSeeOther)
}

// NotificationSubscriptions returns the list of subscribed issues
func NotificationSubscriptions(ctx *context.Context) {
	page := max(ctx.FormInt("page"), 1)

	sortType := ctx.FormString("sort")
	ctx.Data["SortType"] = sortType

	state := ctx.FormString("state")
	if !util.SliceContainsString([]string{"all", "open", "closed"}, state, true) {
		state = "all"
	}

	ctx.Data["State"] = state
	// default state filter is "all"
	showClosed := optional.None[bool]()
	switch state {
	case "closed":
		showClosed = optional.Some(true)
	case "open":
		showClosed = optional.Some(false)
	}

	issueType := ctx.FormString("issueType")
	// default issue type is no filter
	issueTypeBool := optional.None[bool]()
	switch issueType {
	case "issues":
		issueTypeBool = optional.Some(false)
	case "pulls":
		issueTypeBool = optional.Some(true)
	}
	ctx.Data["IssueType"] = issueType

	var labelIDs []int64
	selectedLabels := ctx.FormString("labels")
	ctx.Data["Labels"] = selectedLabels
	if len(selectedLabels) > 0 && selectedLabels != "0" {
		var err error
		labelIDs, err = base.StringsToInt64s(strings.Split(selectedLabels, ","))
		if err != nil {
			ctx.Flash.Error(ctx.Tr("invalid_data", selectedLabels), true)
		}
	}

	count, err := issues_model.CountIssues(ctx, &issues_model.IssuesOptions{
		SubscriberID: ctx.Doer.ID,
		IsClosed:     showClosed,
		IsPull:       issueTypeBool,
		LabelIDs:     labelIDs,
	})
	if err != nil {
		ctx.ServerError("CountIssues", err)
		return
	}
	issues, err := issues_model.Issues(ctx, &issues_model.IssuesOptions{
		Paginator: &db.ListOptions{
			PageSize: setting.UI.IssuePagingNum,
			Page:     page,
		},
		SubscriberID: ctx.Doer.ID,
		SortType:     sortType,
		IsClosed:     showClosed,
		IsPull:       issueTypeBool,
		LabelIDs:     labelIDs,
	})
	if err != nil {
		ctx.ServerError("Issues", err)
		return
	}

	commitStatuses, lastStatus, err := pull_service.GetIssuesAllCommitStatus(ctx, issues)
	if err != nil {
		ctx.ServerError("GetIssuesAllCommitStatus", err)
		return
	}
	if !ctx.Repo.CanRead(unit.TypeActions) {
		for key := range commitStatuses {
			git_model.CommitStatusesHideActionsURL(ctx, commitStatuses[key])
		}
	}
	ctx.Data["CommitLastStatus"] = lastStatus
	ctx.Data["CommitStatuses"] = commitStatuses
	ctx.Data["Issues"] = issues
	ctx.Data["IssueRefEndNames"], ctx.Data["IssueRefURLs"] = issue_service.GetRefEndNamesAndURLs(issues, "")

	approvalCounts, err := issues.GetApprovalCounts(ctx)
	if err != nil {
		ctx.ServerError("ApprovalCounts", err)
		return
	}
	ctx.Data["ApprovalCounts"] = func(issueID int64, typ string) int64 {
		counts, ok := approvalCounts[issueID]
		if !ok || len(counts) == 0 {
			return 0
		}
		reviewTyp := issues_model.ReviewTypeApprove
		switch typ {
		case "reject":
			reviewTyp = issues_model.ReviewTypeReject
		case "waiting":
			reviewTyp = issues_model.ReviewTypeRequest
		}
		for _, count := range counts {
			if count.Type == reviewTyp {
				return count.Count
			}
		}
		return 0
	}

	ctx.Data["Status"] = 1
	ctx.Data["Title"] = ctx.Tr("notification.subscriptions")

	// redirect to last page if request page is more than total pages
	pager := context.NewPagination(int(count), setting.UI.IssuePagingNum, page, 5)
	if pager.Paginater.Current() < page {
		ctx.Redirect(fmt.Sprintf("/notifications/subscriptions?page=%d", pager.Paginater.Current()))
		return
	}
	pager.AddParamFromRequest(ctx.Req)
	ctx.Data["Page"] = pager

	ctx.HTML(http.StatusOK, tplNotificationSubscriptions)
}

// NotificationWatching returns the list of watching repos
func NotificationWatching(ctx *context.Context) {
	page := max(ctx.FormInt("page"), 1)

	keyword := ctx.FormTrim("q")
	ctx.Data["Keyword"] = keyword

	var orderBy db.SearchOrderBy
	ctx.Data["SortType"] = ctx.FormString("sort")
	switch ctx.FormString("sort") {
	case "newest":
		orderBy = db.SearchOrderByNewest
	case "oldest":
		orderBy = db.SearchOrderByOldest
	case "recentupdate":
		orderBy = db.SearchOrderByRecentUpdated
	case "leastupdate":
		orderBy = db.SearchOrderByLeastUpdated
	case "reversealphabetically":
		orderBy = db.SearchOrderByAlphabeticallyReverse
	case "alphabetically":
		orderBy = db.SearchOrderByAlphabetically
	case "moststars":
		orderBy = db.SearchOrderByStarsReverse
	case "feweststars":
		orderBy = db.SearchOrderByStars
	case "mostforks":
		orderBy = db.SearchOrderByForksReverse
	case "fewestforks":
		orderBy = db.SearchOrderByForks
	default:
		ctx.Data["SortType"] = "recentupdate"
		orderBy = db.SearchOrderByRecentUpdated
	}

	archived := ctx.FormOptionalBool("archived")
	ctx.Data["IsArchived"] = archived

	fork := ctx.FormOptionalBool("fork")
	ctx.Data["IsFork"] = fork

	mirror := ctx.FormOptionalBool("mirror")
	ctx.Data["IsMirror"] = mirror

	template := ctx.FormOptionalBool("template")
	ctx.Data["IsTemplate"] = template

	private := ctx.FormOptionalBool("private")
	ctx.Data["IsPrivate"] = private

	repos, count, err := repo_model.SearchRepository(ctx, repo_model.SearchRepoOptions{
		ListOptions: db.ListOptions{
			PageSize: setting.UI.User.RepoPagingNum,
			Page:     page,
		},
		Actor:              ctx.Doer,
		Keyword:            keyword,
		OrderBy:            orderBy,
		Private:            ctx.IsSigned,
		WatchedByID:        ctx.Doer.ID,
		Collaborate:        optional.Some(false),
		TopicOnly:          ctx.FormBool("topic"),
		IncludeDescription: setting.UI.SearchRepoDescription,
		Archived:           archived,
		Fork:               fork,
		Mirror:             mirror,
		Template:           template,
		IsPrivate:          private,
	})
	if err != nil {
		ctx.ServerError("SearchRepository", err)
		return
	}
	total := int(count)
	ctx.Data["Total"] = total
	ctx.Data["Repos"] = repos

	// redirect to last page if request page is more than total pages
	pager := context.NewPagination(total, setting.UI.User.RepoPagingNum, page, 5)
	pager.AddParamFromRequest(ctx.Req)
	ctx.Data["Page"] = pager

	ctx.Data["Status"] = 2
	ctx.Data["Title"] = ctx.Tr("notification.watching")

	ctx.HTML(http.StatusOK, tplNotificationSubscriptions)
}

// NewAvailable returns the notification counts
func NewAvailable(ctx *context.Context) {
	total, err := db.Count[activities_model.Notification](ctx, activities_model.FindNotificationOptions{
		UserID: ctx.Doer.ID,
		Status: []activities_model.NotificationStatus{activities_model.NotificationStatusUnread},
	})
	if err != nil {
		log.Error("db.Count[activities_model.Notification]", err)
		ctx.JSON(http.StatusOK, structs.NotificationCount{New: 0})
		return
	}

	ctx.JSON(http.StatusOK, structs.NotificationCount{New: total})
}
