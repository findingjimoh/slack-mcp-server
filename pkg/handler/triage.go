package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/text"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const maxTriageDMs = 200
const maxSearchPages = 20
const searchPageSize = 50

// TriageHandler orchestrates a full Slack triage in a single MCP tool call.
type TriageHandler struct {
	apiProvider *provider.ApiProvider
	logger      *zap.Logger
}

func NewTriageHandler(apiProvider *provider.ApiProvider, logger *zap.Logger) *TriageHandler {
	return &TriageHandler{
		apiProvider: apiProvider,
		logger:      logger,
	}
}

// --- Output types (JSON) ---

type TriageResult struct {
	MyUserID    string          `json:"my_user_id"`
	MyUserName  string          `json:"my_username"`
	FetchedAt   string          `json:"fetched_at"`
	DMs         []TriageDM      `json:"dms"`
	Mentions    []TriageMention `json:"mentions"`
	ThreadReply []TriageMention `json:"thread_replies"`
	Stats       TriageStats     `json:"stats"`
}

type TriageDM struct {
	ChannelID   string          `json:"channel_id"`
	ChannelName string          `json:"channel_name"`
	IsBot       bool            `json:"is_bot"`
	Messages    []TriageMessage `json:"messages"`
}

type TriageMessage struct {
	MsgID      string `json:"msg_id"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
	Time       string `json:"time"`
	ThreadTs   string `json:"thread_ts,omitempty"`
	Reactions  string `json:"reactions,omitempty"`
}

type TriageMention struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	SenderID    string `json:"sender_id"`
	SenderName  string `json:"sender_name"`
	MsgID       string `json:"msg_id"`
	Text        string `json:"text"`
	Time        string `json:"time"`
	ThreadTs    string `json:"thread_ts,omitempty"`
}

type TriageStats struct {
	TotalUnreadConversations      int `json:"total_unread_conversations"`
	DMsNeedingAttention           int `json:"dms_needing_attention"`
	MentionsNeedingAttention      int `json:"mentions_needing_attention"`
	ThreadRepliesNeedingAttention int `json:"thread_replies_needing_attention"`
}

// TriageUnreadsHandler is the MCP tool handler.
func (th *TriageHandler) TriageUnreadsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	th.logger.Info("TriageUnreadsHandler called")

	// Feature flag check
	toolConfig := os.Getenv("SLACK_MCP_TRIAGE_TOOL")
	if toolConfig == "" {
		return nil, errors.New(
			"by default, the slack_triage tool is disabled. " +
				"To enable it, set the SLACK_MCP_TRIAGE_TOOL environment variable to true or 1",
		)
	}
	if toolConfig != "true" && toolConfig != "1" && toolConfig != "yes" {
		return nil, errors.New("SLACK_MCP_TRIAGE_TOOL must be set to 'true', '1', or 'yes' to enable")
	}

	if ready, err := th.apiProvider.IsReady(); !ready {
		th.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	// Parse params
	lookbackDaysStr := request.GetString("lookback_days", "3")
	lookbackDays, err := strconv.Atoi(lookbackDaysStr)
	if err != nil || lookbackDays < 1 {
		lookbackDays = 3
	}
	includeMentions := request.GetBool("include_mentions", true)
	includeThreadReplies := request.GetBool("include_thread_replies", true)

	// Step 1: Get my user ID
	ar, err := th.apiProvider.Slack().AuthTest()
	if err != nil {
		th.logger.Error("AuthTest failed", zap.Error(err))
		return nil, err
	}
	myUserID := ar.UserID
	myUserName := ar.User
	th.logger.Info("Triage for user", zap.String("user_id", myUserID), zap.String("username", myUserName))

	// Step 2: Get unread counts
	counts, err := th.apiProvider.GetClientCounts(ctx)
	if err != nil {
		th.logger.Error("GetClientCounts failed", zap.Error(err))
		return nil, err
	}

	// Step 3: Build set of unread IM/MPIM channel IDs
	channelsCache := th.apiProvider.ProvideChannelsMaps()
	usersCache := th.apiProvider.ProvideUsersMap()

	type unreadInfo struct {
		channelID  string
		lastReadTs string
		latestTs   string
	}
	var unreadDMs []unreadInfo

	// Collect unread IMs
	for _, snap := range counts.IMs {
		if !snap.HasUnreads {
			continue
		}
		unreadDMs = append(unreadDMs, unreadInfo{
			channelID:  snap.ID,
			lastReadTs: snap.LastRead.SlackString(),
			latestTs:   snap.Latest.SlackString(),
		})
	}
	// Collect unread MPIMs
	for _, snap := range counts.MPIMs {
		if !snap.HasUnreads {
			continue
		}
		unreadDMs = append(unreadDMs, unreadInfo{
			channelID:  snap.ID,
			lastReadTs: snap.LastRead.SlackString(),
			latestTs:   snap.Latest.SlackString(),
		})
	}

	// Build channel→lastReadTs map for read-status filtering of mentions/threads
	readCursors := make(map[string]string, len(counts.IMs)+len(counts.Channels)+len(counts.MPIMs))
	for _, snap := range counts.IMs {
		readCursors[snap.ID] = snap.LastRead.SlackString()
	}
	for _, snap := range counts.Channels {
		readCursors[snap.ID] = snap.LastRead.SlackString()
	}
	for _, snap := range counts.MPIMs {
		readCursors[snap.ID] = snap.LastRead.SlackString()
	}

	th.logger.Info("Unread DMs/MPIMs found", zap.Int("count", len(unreadDMs)))

	// Sort by latestTs descending and cap at maxTriageDMs
	sort.Slice(unreadDMs, func(i, j int) bool {
		return unreadDMs[i].latestTs > unreadDMs[j].latestTs
	})
	if len(unreadDMs) > maxTriageDMs {
		unreadDMs = unreadDMs[:maxTriageDMs]
	}

	// Step 4: Fetch history and apply heuristic for each unread DM/MPIM
	var triageDMs []TriageDM
	// Track seen message keys for deduplication
	seenMessages := make(map[string]bool) // "channelID:msgTS"
	// Track DM channels where the latest message is from me (self-replied)
	selfRepliedDMs := make(map[string]bool)

	for _, dm := range unreadDMs {
		if err := ctx.Err(); err != nil {
			th.logger.Warn("Context cancelled during DM fetch", zap.Error(err))
			break
		}

		historyParams := slack.GetConversationHistoryParameters{
			ChannelID: dm.channelID,
			Oldest:    dm.lastReadTs,
			Limit:     50,
			Inclusive: false,
		}
		history, err := th.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
		if err != nil {
			th.logger.Warn("Failed to fetch history for DM",
				zap.String("channel", dm.channelID), zap.Error(err))
			continue
		}

		if len(history.Messages) == 0 {
			continue
		}

		// Self-reply filter: if the latest message is from me, skip the entire DM.
		// Mark all messages as seen so the mentions search won't re-surface them.
		if history.Messages[0].User == myUserID {
			selfRepliedDMs[dm.channelID] = true
			for _, msg := range history.Messages {
				seenMessages[dm.channelID+":"+msg.Timestamp] = true
			}
			continue
		}

		// Apply heuristic to each message
		var actionableMessages []TriageMessage
		for i, msg := range history.Messages {
			// Mark as seen for dedup against mentions search (even if not actionable)
			seenMessages[dm.channelID+":"+msg.Timestamp] = true

			if msg.User == myUserID {
				continue
			}
			if msg.SubType != "" && msg.SubType != "bot_message" && msg.SubType != "thread_broadcast" {
				continue
			}

			var nextMsg *slack.Message
			if i > 0 {
				// Messages are newest-first, so i-1 is more recent
				nextMsg = &history.Messages[i-1]
			}

			if !needsAttention(msg, myUserID, nextMsg) {
				continue
			}

			// For threaded messages, check if I replied in the thread
			if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
				if th.hasRepliedInThread(ctx, dm.channelID, msg.ThreadTimestamp, myUserID) {
					continue
				}
			}

			ts, _ := text.TimestampToIsoRFC3339(msg.Timestamp)
			senderName, _, _ := getUserInfo(msg.User, usersCache.Users)

			var reactionParts []string
			for _, r := range msg.Reactions {
				reactionParts = append(reactionParts, fmt.Sprintf("%s:%d", r.Name, r.Count))
			}

			actionableMessages = append(actionableMessages, TriageMessage{
				MsgID:      msg.Timestamp,
				SenderID:   msg.User,
				SenderName: senderName,
				Text:       text.ProcessText(msg.Text + text.AttachmentsTo2CSV(msg.Text, msg.Attachments)),
				Time:       ts,
				ThreadTs:   msg.ThreadTimestamp,
				Reactions:  strings.Join(reactionParts, "|"),
			})
		}

		if len(actionableMessages) == 0 {
			continue
		}

		// Determine channel name and bot status
		channelName := dm.channelID
		isBot := false
		if ch, ok := channelsCache.Channels[dm.channelID]; ok {
			channelName = ch.Name
			if ch.IsIM && ch.User != "" {
				if u, ok := usersCache.Users[ch.User]; ok {
					isBot = u.IsBot
				}
			}
		}

		triageDMs = append(triageDMs, TriageDM{
			ChannelID:   dm.channelID,
			ChannelName: channelName,
			IsBot:       isBot,
			Messages:    actionableMessages,
		})
	}

	// Step 5: Search @mentions (if enabled)
	var mentions []TriageMention
	if includeMentions && !th.apiProvider.IsBotToken() {
		mentions = th.searchMentions(ctx, myUserID, myUserName, lookbackDays, seenMessages, readCursors, selfRepliedDMs)
	}

	// Step 5b: Reconcile mentions against conversations_counts.
	// The search API (to:me) can miss mentions that Slack tracks via MentionCount.
	// For channels with MentionCount > 0 not already covered by search results,
	// fetch history and scan for <@userID> mentions.
	if includeMentions && !th.apiProvider.IsBotToken() {
		mentionedChannels := make(map[string]bool, len(mentions))
		for _, m := range mentions {
			mentionedChannels[m.ChannelID] = true
		}

		mentionTag := fmt.Sprintf("<@%s>", myUserID)

		reconcileSnapshots := func(snapshots []struct {
			ID           string
			MentionCount int
			HasUnreads   bool
			LastRead     string
		}) {
			for _, snap := range snapshots {
				if snap.MentionCount == 0 || !snap.HasUnreads {
					continue
				}
				if mentionedChannels[snap.ID] {
					continue
				}

				historyParams := slack.GetConversationHistoryParameters{
					ChannelID: snap.ID,
					Oldest:    snap.LastRead,
					Limit:     50,
					Inclusive: false,
				}
				history, err := th.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
				if err != nil {
					th.logger.Warn("Failed to fetch history for missed mention channel",
						zap.String("channel", snap.ID), zap.Error(err))
					continue
				}

				for _, msg := range history.Messages {
					if msg.User == myUserID {
						continue
					}
					if seenMessages[snap.ID+":"+msg.Timestamp] {
						continue
					}
					if !strings.Contains(msg.Text, mentionTag) {
						continue
					}

					threadTs := msg.ThreadTimestamp
					if threadTs != "" && threadTs != msg.Timestamp {
						if th.hasRepliedInThread(ctx, snap.ID, threadTs, myUserID) {
							continue
						}
					}

					ts, _ := text.TimestampToIsoRFC3339(msg.Timestamp)
					senderName, _, _ := getUserInfo(msg.User, usersCache.Users)
					channelName := snap.ID
					if ch, ok := channelsCache.Channels[snap.ID]; ok {
						channelName = "#" + ch.Name
					}

					mentions = append(mentions, TriageMention{
						ChannelID:   snap.ID,
						ChannelName: channelName,
						SenderID:    msg.User,
						SenderName:  senderName,
						MsgID:       msg.Timestamp,
						Text:        text.ProcessText(msg.Text + text.AttachmentsTo2CSV(msg.Text, msg.Attachments)),
						Time:        ts,
						ThreadTs:    threadTs,
					})
					seenMessages[snap.ID+":"+msg.Timestamp] = true
				}
			}
		}

		// Normalize channel and MPIM snapshots into a common shape
		var toReconcile []struct {
			ID           string
			MentionCount int
			HasUnreads   bool
			LastRead     string
		}
		for _, snap := range counts.Channels {
			toReconcile = append(toReconcile, struct {
				ID           string
				MentionCount int
				HasUnreads   bool
				LastRead     string
			}{snap.ID, snap.MentionCount, snap.HasUnreads, snap.LastRead.SlackString()})
		}
		for _, snap := range counts.MPIMs {
			toReconcile = append(toReconcile, struct {
				ID           string
				MentionCount int
				HasUnreads   bool
				LastRead     string
			}{snap.ID, snap.MentionCount, snap.HasUnreads, snap.LastRead.SlackString()})
		}
		reconcileSnapshots(toReconcile)

		if reconciledCount := len(mentions) - len(mentionedChannels); reconciledCount > 0 {
			th.logger.Info("Reconciled additional mentions from conversations_counts",
				zap.Int("reconciled", reconciledCount))
		}
	}

	// Step 6: Thread reply sweep (if enabled)
	var threadReplies []TriageMention
	if includeThreadReplies && !th.apiProvider.IsBotToken() {
		threadReplies = th.searchThreadReplies(ctx, myUserID, myUserName, lookbackDays, seenMessages, readCursors, selfRepliedDMs)
	}

	// Build result
	result := TriageResult{
		MyUserID:    myUserID,
		MyUserName:  myUserName,
		FetchedAt:   time.Now().UTC().Format(time.RFC3339),
		DMs:         triageDMs,
		Mentions:    mentions,
		ThreadReply: threadReplies,
		Stats: TriageStats{
			TotalUnreadConversations:      len(unreadDMs),
			DMsNeedingAttention:           len(triageDMs),
			MentionsNeedingAttention:      len(mentions),
			ThreadRepliesNeedingAttention: len(threadReplies),
		},
	}

	jsonBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		th.logger.Error("Failed to marshal triage result", zap.Error(err))
		return nil, err
	}

	th.logger.Info("Triage complete",
		zap.Int("dms", len(triageDMs)),
		zap.Int("mentions", len(mentions)),
		zap.Int("thread_replies", len(threadReplies)),
	)

	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// needsAttention determines if a message requires the user's attention.
// Returns false if the user has already addressed it (reacted, replied, etc).
func needsAttention(msg slack.Message, myUserID string, nextMsg *slack.Message) bool {
	// Skip if I reacted (except 👀 which means "looking into it")
	for _, r := range msg.Reactions {
		for _, uid := range r.Users {
			if uid == myUserID {
				if r.Name != "eyes" {
					return false
				}
			}
		}
	}

	// Skip if I replied immediately after (nextMsg is more recent)
	if nextMsg != nil && nextMsg.User == myUserID {
		return false
	}

	return true
}

// hasRepliedInThread checks if the user has replied in a given thread.
func (th *TriageHandler) hasRepliedInThread(ctx context.Context, channelID, threadTs, myUserID string) bool {
	repliesParams := slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTs,
		Limit:     100,
	}
	replies, _, _, err := th.apiProvider.Slack().GetConversationRepliesContext(ctx, &repliesParams)
	if err != nil {
		th.logger.Warn("Failed to fetch thread replies",
			zap.String("channel", channelID),
			zap.String("thread_ts", threadTs),
			zap.Error(err))
		return false
	}

	for _, reply := range replies {
		if reply.User == myUserID && reply.Timestamp != threadTs {
			return true
		}
	}
	return false
}

// paginatedSearch runs a Slack search query with pagination (up to maxSearchPages)
// and filters results using the standard triage heuristics.
func (th *TriageHandler) paginatedSearch(ctx context.Context, query, label string, myUserID string, seen map[string]bool, readCursors map[string]string, selfRepliedDMs map[string]bool) []TriageMention {
	th.logger.Debug("Searching "+label, zap.String("query", query))

	usersCache := th.apiProvider.ProvideUsersMap()
	var results []TriageMention

	for page := 1; page <= maxSearchPages; page++ {
		if err := ctx.Err(); err != nil {
			th.logger.Warn("Context cancelled during "+label+" search", zap.Error(err))
			break
		}

		searchParams := slack.SearchParameters{
			Sort:          slack.DEFAULT_SEARCH_SORT,
			SortDirection: slack.DEFAULT_SEARCH_SORT_DIR,
			Highlight:     false,
			Count:         searchPageSize,
			Page:          page,
		}
		messagesRes, _, err := th.apiProvider.Slack().SearchContext(ctx, query, searchParams)
		if err != nil {
			th.logger.Warn(label+" search failed", zap.Error(err), zap.Int("page", page))
			break
		}

		for _, msg := range messagesRes.Matches {
			if msg.User == myUserID {
				continue
			}
			channelID := msg.Channel.ID
			if seen[channelID+":"+msg.Timestamp] {
				continue
			}
			if lastRead, ok := readCursors[channelID]; ok && msg.Timestamp <= lastRead {
				continue
			}

			// Self-reply filter for DM channels: skip if I sent the latest message
			if strings.HasPrefix(channelID, "D") {
				if replied, cached := selfRepliedDMs[channelID]; cached {
					if replied {
						continue
					}
				} else {
					// Not seen in DM loop — do a live check
					latestParams := slack.GetConversationHistoryParameters{
						ChannelID: channelID,
						Limit:     1,
					}
					if lh, lhErr := th.apiProvider.Slack().GetConversationHistoryContext(ctx, &latestParams); lhErr == nil && len(lh.Messages) > 0 {
						selfRepliedDMs[channelID] = lh.Messages[0].User == myUserID
						if selfRepliedDMs[channelID] {
							continue
						}
					}
				}
			}

			threadTs, _ := extractThreadTS(msg.Permalink)
			if threadTs != "" {
				if th.hasRepliedInThread(ctx, channelID, threadTs, myUserID) {
					continue
				}
			}

			ts, _ := text.TimestampToIsoRFC3339(msg.Timestamp)
			senderName, _, _ := getUserInfo(msg.User, usersCache.Users)

			results = append(results, TriageMention{
				ChannelID:   channelID,
				ChannelName: "#" + msg.Channel.Name,
				SenderID:    msg.User,
				SenderName:  senderName,
				MsgID:       msg.Timestamp,
				Text:        text.ProcessText(msg.Text + text.AttachmentsTo2CSV(msg.Text, msg.Attachments)),
				Time:        ts,
				ThreadTs:    threadTs,
			})
			seen[channelID+":"+msg.Timestamp] = true
		}

		if len(messagesRes.Matches) < searchPageSize {
			break // no more pages
		}
	}

	th.logger.Info(label+" found", zap.Int("count", len(results)))
	return results
}

// searchMentions searches for @mentions needing attention.
func (th *TriageHandler) searchMentions(ctx context.Context, myUserID, myUserName string, lookbackDays int, seen map[string]bool, readCursors map[string]string, selfRepliedDMs map[string]bool) []TriageMention {
	afterDate := time.Now().AddDate(0, 0, -lookbackDays).Format("2006-01-02")
	query := fmt.Sprintf("to:me after:%s", afterDate)
	return th.paginatedSearch(ctx, query, "Mentions", myUserID, seen, readCursors, selfRepliedDMs)
}

// searchThreadReplies does a supplementary sweep for thread replies involving me.
func (th *TriageHandler) searchThreadReplies(ctx context.Context, myUserID, myUserName string, lookbackDays int, seen map[string]bool, readCursors map[string]string, selfRepliedDMs map[string]bool) []TriageMention {
	afterDate := time.Now().AddDate(0, 0, -lookbackDays).Format("2006-01-02")
	query := fmt.Sprintf("with:@%s is:thread after:%s", myUserName, afterDate)
	return th.paginatedSearch(ctx, query, "Thread replies", myUserID, seen, readCursors, selfRepliedDMs)
}
