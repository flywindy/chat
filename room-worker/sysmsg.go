package main

import (
	"github.com/hmchangw/chat/pkg/displayfmt"
	"github.com/hmchangw/chat/pkg/model"
)

func displayName(u *model.User) string {
	return displayfmt.CombineWithFallback(u.EngName, u.ChineseName, u.Account)
}

func displayOrg(name, tcName, orgID string) string {
	return displayfmt.CombineWithFallback(name, tcName, orgID)
}

func quoted(name string) string {
	return "\"" + name + "\""
}

func formatAddedSingle(requester, added *model.User) string {
	return quoted(displayName(requester)) + " added " + quoted(displayName(added)) + " to the channel"
}

func formatAddedMulti(requester *model.User) string {
	return quoted(displayName(requester)) + " added members to the channel"
}

func formatRemovedUser(user *model.User) string {
	return quoted(displayName(user)) + " has been removed from the channel"
}

func formatRemovedOrg(name, tcName, orgID string) string {
	return quoted(displayOrg(name, tcName, orgID)) + " has been removed from the channel"
}

func formatLeft(user *model.User) string {
	return quoted(displayName(user)) + " left the channel"
}
