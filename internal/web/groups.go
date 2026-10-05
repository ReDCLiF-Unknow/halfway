package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"halfway/internal/discord"
	"halfway/internal/store"
)

// Groups: people who meet again and again. A group's page lists its polls,
// who is in it, and how the travelling has been shared out; its invite link
// puts people in the group, and so on every poll it makes.

func (s *Server) groupRoutes() {
	s.mux.HandleFunc("POST /groups", s.authed(s.formCreateGroup))
	s.mux.HandleFunc("GET /groups/{id}", s.groupMember(s.pageGroup))
	s.mux.HandleFunc("POST /groups/{id}/leave", s.groupMember(s.formLeaveGroup))
	s.mux.HandleFunc("POST /groups/{id}/rename", s.groupOrganizer(s.formRenameGroup))
	s.mux.HandleFunc("POST /groups/{id}/invite/reset", s.groupOrganizer(s.formResetGroupInvite))
	s.mux.HandleFunc("POST /groups/{id}/delete", s.groupOrganizer(s.formDeleteGroup))
	s.mux.HandleFunc("POST /groups/{id}/people/{user}/remove", s.groupOrganizer(s.formRemoveMember))
	s.mux.HandleFunc("POST /groups/{id}/chats/discord", s.groupOrganizer(s.formGroupDiscord))
	s.mux.HandleFunc("POST /groups/{id}/chats/{chat}/remove", s.groupOrganizer(s.formRemoveGroupChat))
	s.mux.HandleFunc("GET /g/{code}", s.groupInviteGet)
	s.mux.HandleFunc("POST /g/{code}", s.groupInvitePost)
}

type groupCtx struct {
	User  *store.User
	Group store.Group
}

type groupHandler func(http.ResponseWriter, *http.Request, groupCtx)

func groupPath(id int64) string { return "/groups/" + strconv.FormatInt(id, 10) }

func groupInviteURL(r *http.Request, g store.Group) string { return baseURL(r) + "/g/" + g.InviteCode }

// groupMember lets through the people in a group; to anyone else it does
// not exist.
func (s *Server) groupMember(h groupHandler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, ok := pathID(r, "id")
		if !ok {
			http.NotFound(w, r)
			return
		}
		g, err := s.store.Group(id, u.ID)
		if err != nil {
			s.htmlErr(w, r, err)
			return
		}
		h(w, r, groupCtx{User: u, Group: g})
	})
}

func (s *Server) groupOrganizer(h groupHandler) http.HandlerFunc {
	return s.groupMember(func(w http.ResponseWriter, r *http.Request, c groupCtx) {
		if !c.Group.Organizer {
			http.Error(w, "only the group's organizers can do that", http.StatusForbidden)
			return
		}
		h(w, r, c)
	})
}

// groupData is a group's page.
type groupData struct {
	Members   []store.Member
	Polls     []store.Summary
	Shares    []shareRow
	Chats     []store.Chat
	InviteURL string
	Telegram  string
	// Counted is whether any meetup has taught the memory anything yet.
	Counted bool
}

// shareRow is one member in the "who has travelled most" table.
type shareRow struct {
	store.Share
	Rounded int // Extra in whole minutes
	Bar     int // how wide its bar is, as a percentage
}

func (s *Server) formCreateGroup(w http.ResponseWriter, r *http.Request, u *store.User) {
	g, err := s.store.CreateGroup(u.ID, r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	// Straight to its invite link: asking people in is the next thing to do.
	http.Redirect(w, r, groupPath(g.ID)+"#share", http.StatusSeeOther)
}

func (s *Server) pageGroup(w http.ResponseWriter, r *http.Request, c groupCtx) {
	d, err := s.basePage(r, c.User, "group")
	if err != nil {
		s.fail(w, err)
		return
	}
	g := c.Group
	d.Group = &g
	gd := groupData{InviteURL: groupInviteURL(r, g)}
	if gd.Members, err = s.store.Members(g.ID); err != nil {
		s.fail(w, err)
		return
	}
	if gd.Polls, err = s.store.GroupPolls(g.ID, c.User.ID); err != nil {
		s.fail(w, err)
		return
	}
	shares, err := s.store.Shares(g.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	most := 1.0
	for _, sh := range shares {
		most = math.Max(most, math.Abs(sh.Extra))
		gd.Counted = gd.Counted || sh.Meetups > 0
	}
	for _, sh := range shares {
		gd.Shares = append(gd.Shares, shareRow{Share: sh, Rounded: int(math.Round(sh.Extra)), Bar: int(math.Abs(sh.Extra) / most * 50)})
	}
	if g.Organizer {
		if gd.Chats, err = s.store.GroupChats(g.ID); err != nil {
			s.fail(w, err)
			return
		}
		if s.bot != nil {
			gd.Telegram = s.bot.AddLink(g.ChatCode)
		}
	}
	d.GroupPage = gd
	w.Header().Set("Cache-Control", "no-store")
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) formLeaveGroup(w http.ResponseWriter, r *http.Request, c groupCtx) {
	if err := s.store.LeaveGroup(c.Group.ID, c.User.ID); errors.Is(err, store.ErrInvalid) {
		http.Error(w, "you are this group's only organizer: make someone else one first, or delete it", http.StatusConflict)
		return
	} else if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) formRenameGroup(w http.ResponseWriter, r *http.Request, c groupCtx) {
	if err := s.store.RenameGroup(c.Group.ID, r.FormValue("name")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, groupPath(c.Group.ID)), http.StatusSeeOther)
}

func (s *Server) formResetGroupInvite(w http.ResponseWriter, r *http.Request, c groupCtx) {
	if err := s.store.ResetGroupInvite(c.Group.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, groupPath(c.Group.ID)), http.StatusSeeOther)
}

func (s *Server) formDeleteGroup(w http.ResponseWriter, r *http.Request, c groupCtx) {
	if err := s.store.DeleteGroup(c.Group.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) formRemoveMember(w http.ResponseWriter, r *http.Request, c groupCtx) {
	uid, ok := pathID(r, "user")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.LeaveGroup(c.Group.ID, uid); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, groupPath(c.Group.ID)), http.StatusSeeOther)
}

func (s *Server) formGroupDiscord(w http.ResponseWriter, r *http.Request, c groupCtx) {
	if s.discord == nil {
		http.NotFound(w, r)
		return
	}
	hook, err := s.discord.Clean(r.FormValue("webhook"))
	if err != nil {
		http.Error(w, "That isn't a Discord webhook URL. In Discord: the channel's settings, Integrations, Webhooks, Copy Webhook URL.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	name, err := s.discord.Check(ctx, hook)
	if errors.Is(err, discord.ErrRefused) {
		http.Error(w, "Discord says that webhook doesn't exist. Copy its URL again.", http.StatusBadRequest)
		return
	} else if err != nil {
		log.Printf("discord: %v", err)
		http.Error(w, "Couldn't reach Discord. Try again in a moment.", http.StatusBadGateway)
		return
	}
	title := "Discord"
	if name != "" {
		title = "Discord: " + name
	}
	if err := s.store.AddGroupChat(c.Group.ID, discord.Platform, hook, title); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, groupPath(c.Group.ID)), http.StatusSeeOther)
}

func (s *Server) formRemoveGroupChat(w http.ResponseWriter, r *http.Request, c groupCtx) {
	id, ok := pathID(r, "chat")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RemoveGroupChat(c.Group.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, groupPath(c.Group.ID)), http.StatusSeeOther)
}

// groupJoinPage is what a group's invite link shows somebody not in it yet.
func (s *Server) groupJoinPage(r *http.Request, g store.Group, errMsg string) simplePage {
	members, _ := s.store.Members(g.ID)
	var people []store.Participant
	for _, m := range members {
		people = append(people, store.Participant{ID: m.ID, Name: m.Name})
	}
	status := fmt.Sprintf("A group of %d. You'll be on every poll it makes.", len(members))
	return simplePage{
		Title:     "Join " + g.Name,
		Heading:   "Join “" + g.Name + "”",
		Subtitle:  "A group that meets again and again. Pick a name so the others can tell who's who.",
		Action:    "/g/" + g.InviteCode,
		Next:      "/g/" + g.InviteCode,
		Button:    "Join group",
		Error:     errMsg,
		NeedName:  userFrom(r) == nil,
		ShowToken: userFrom(r) == nil,
		Invite:    &invitePreview{Title: g.Name, Category: "group", Status: status, People: people},
		OG:        &openGraph{Title: g.Name, Description: status, URL: groupInviteURL(r, g)},
	}
}

// groupInviteGet is where a group's invite link leads. Somebody with a name
// joins at once; anybody else is asked for one first.
func (s *Server) groupInviteGet(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var viewer int64
	if u != nil {
		viewer = u.ID
	}
	g, err := s.store.GroupByInvite(r.PathValue("code"), viewer)
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneLink())
		return
	}
	if u != nil {
		if err := s.store.JoinGroup(g.ID, u.ID); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, groupPath(g.ID), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", s.groupJoinPage(r, g, ""))
}

func (s *Server) groupInvitePost(w http.ResponseWriter, r *http.Request) {
	g, err := s.store.GroupByInvite(r.PathValue("code"), 0)
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneLink())
		return
	}
	u := userFrom(r)
	if u == nil {
		if !s.signups.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html", s.groupJoinPage(r, g, "Too many new names from this connection. Try again in a minute."))
			return
		}
		nu, token, err := s.store.CreateUser(r.FormValue("name"))
		if errors.Is(err, store.ErrInvalid) {
			s.renderTmpl(w, http.StatusBadRequest, "welcome.html", s.groupJoinPage(r, g, "Please enter a name (up to 40 characters)."))
			return
		} else if err != nil {
			s.fail(w, err)
			return
		}
		setSession(w, r, token)
		u = &nu
	}
	if err := s.store.JoinGroup(g.ID, u.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, groupPath(g.ID), http.StatusSeeOther)
}
