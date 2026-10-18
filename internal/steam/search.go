package steam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"mime"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

const maxSearchResponseBytes int64 = 2 << 20

// These selectors and tooltip grammar describe the unversioned public Store
// results surface, not a documented Steamworks API. Fixture changes must be
// reviewed as upstream protocol changes; unknown layout is never an empty feed.
var (
	searchReviewSentence = regexp.MustCompile(`^([0-9]{1,3})% of the ([0-9]+|[1-9][0-9]{0,2}(?:,[0-9]{3})+) user reviews for this game are positive\.$`)
	searchZeroSentence   = regexp.MustCompile(`^0 user reviews\.?$`)
)

func (c *HTTPClient) Search(ctx context.Context, request SearchRequest) (SearchPage, error) {
	if request.Start < 0 || request.Count < 1 || request.Count > 200 || request.Start > math.MaxInt-request.Count {
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	filter, sort := "", ""
	switch request.Feed {
	case FeedPopularUpcoming:
		filter = "popularcomingsoon"
	case FeedPopularNew:
		filter = "popularnew"
	case FeedRecent:
		filter, sort = "recent", "Released_DESC"
	case FeedComingSoon:
		filter, sort = "comingsoon", "Released_ASC"
	default:
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	values := url.Values{
		"filter": {filter}, "l": {"english"}, "cc": {"us"}, "category1": {"998"},
		"ignore_preferences": {"1"}, "infinite": {"1"},
		"start": {strconv.Itoa(request.Start)}, "count": {strconv.Itoa(request.Count)},
	}
	if sort != "" {
		values.Set("sort_by", sort)
	}
	response, _, err := c.getWithoutCache(ctx, CacheKey{Operation: SearchOperation}, c.storeBase, "/search/results/", values)
	if err != nil {
		return SearchPage{}, err
	}
	mediaType, _, err := mime.ParseMediaType(response.header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	return parseSearchPage(response.body, request)
}

func parseSearchPage(body []byte, request SearchRequest) (SearchPage, error) {
	var payload struct {
		Success     *int            `json:"success"`
		Start       json.RawMessage `json:"start"`
		PageSize    json.RawMessage `json:"pagesize"`
		Total       *int            `json:"total_count"`
		ResultsHTML *string         `json:"results_html"`
	}
	if err := decode(body, &payload, SearchOperation); err != nil {
		return SearchPage{}, err
	}
	start, ok := searchPageInteger(payload.Start)
	if payload.Success == nil || *payload.Success != 1 || !ok || start != request.Start || payload.Total == nil || *payload.Total < 0 || payload.ResultsHTML == nil {
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	pageSize := request.Count
	if len(payload.PageSize) != 0 {
		if count, valid := searchPageInteger(payload.PageSize); !valid || count > request.Count {
			return SearchPage{}, invalidResponse(SearchOperation)
		} else {
			pageSize = count
		}
	}
	page := SearchPage{Items: make([]SearchObservation, 0), Start: start, Total: *payload.Total}
	markup := strings.TrimSpace(*payload.ResultsHTML)
	if markup == "" {
		if start < page.Total {
			return SearchPage{}, invalidResponse(SearchOperation)
		}
		return page, nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(markup))
	if err != nil {
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	rows := doc.Find("a.search_result_row")
	if rows.Length() == 0 || rows.Length() > pageSize || rows.Length() > max(0, page.Total-start) {
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	page.Count = rows.Length()
	page.HasMore = start+page.Count < page.Total
	seen := make(map[int64]SearchObservation, rows.Length())
	valid := true
	rows.EachWithBreak(func(index int, row *goquery.Selection) bool {
		item, err := parseSearchRow(row, start+index)
		if err != nil {
			valid = false
			return false
		}
		if previous, exists := seen[item.AppID]; exists {
			// Reviews intentionally do not affect the material fingerprint, so
			// compare them separately. Any conflicting duplicate rejects this
			// page instead of letting row order drop rejection evidence or
			// advance the cursor with an ambiguous review snapshot.
			if previous.Fingerprint != item.Fingerprint || !sameSearchReview(previous.Review, item.Review) {
				valid = false
				return false
			}
			return true
		}
		seen[item.AppID] = item
		page.Items = append(page.Items, item)
		return true
	})
	if !valid {
		return SearchPage{}, invalidResponse(SearchOperation)
	}
	return page, nil
}

func sameSearchReview(a, b ReviewSummary) bool {
	if a.Band != b.Band || a.Label != b.Label || a.Total != b.Total {
		return false
	}
	if a.PositivePercent == nil || b.PositivePercent == nil {
		return a.PositivePercent == nil && b.PositivePercent == nil
	}
	return *a.PositivePercent == *b.PositivePercent
}

func parseSearchRow(row *goquery.Selection, position int) (SearchObservation, error) {
	rawID, present := row.Attr("data-ds-appid")
	appID, err := strconv.ParseUint(rawID, 10, 32)
	if !present || err != nil || appID == 0 || strconv.FormatUint(appID, 10) != rawID {
		return SearchObservation{}, invalidResponse(SearchOperation)
	}
	href, _ := row.Attr("href")
	link, err := url.Parse(href)
	if err != nil || link.Scheme != "https" || link.Host != "store.steampowered.com" || link.User != nil || !strings.HasPrefix(link.Path, "/app/"+rawID+"/") {
		return SearchObservation{}, invalidResponse(SearchOperation)
	}
	title, released := row.Find(".title"), row.Find(".search_released")
	if title.Length() != 1 || released.Length() != 1 {
		return SearchObservation{}, invalidResponse(SearchOperation)
	}
	name, nameOK := normalizedSearchText(title.Text(), 512)
	release, releaseOK := normalizedSearchText(released.Text(), 512)
	if !nameOK || !releaseOK || name == "" {
		return SearchObservation{}, invalidResponse(SearchOperation)
	}
	item := SearchObservation{AppID: int64(appID), Name: name, ReleaseText: release, Position: position, Review: parseSearchReview(row)}
	// Length framing through JSON avoids ambiguous concatenated identities.
	identity, _ := json.Marshal([]any{item.AppID, item.Name, item.ReleaseText})
	hash := sha256.Sum256(identity)
	item.Fingerprint = hex.EncodeToString(hash[:])
	return item, nil
}

func parseSearchReview(row *goquery.Selection) ReviewSummary {
	unknown := ReviewSummary{Band: ReviewUnknown}
	reviews := row.Find(".search_review_summary")
	if reviews.Length() != 1 {
		return unknown
	}
	tooltip, ok := reviews.Attr("data-tooltip-html")
	if !ok || len(tooltip) > 2048 {
		return unknown
	}
	// Parse only this bounded aggregate tooltip. It is discarded immediately;
	// no HTML, review text, price, or personalization enters the read model.
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(tooltip))
	if err != nil || doc.Find("script, style, iframe, img, a").Length() > 0 {
		return unknown
	}
	doc.Find("br").Each(func(_ int, br *goquery.Selection) { br.ReplaceWithHtml("\n") })
	lines := strings.Split(doc.Find("body").Text(), "\n")
	if len(lines) > 2 {
		return unknown
	}
	label := strings.TrimSpace(lines[0])
	if len(lines) == 1 {
		if label == "No user reviews" {
			return NormalizeReview(label, nil, nil)
		}
		return unknown
	}
	sentence := strings.TrimSpace(lines[1])
	if label == "No user reviews" && searchZeroSentence.MatchString(sentence) {
		zero := 0
		return NormalizeReview(label, nil, &zero)
	}
	match := searchReviewSentence.FindStringSubmatch(sentence)
	if match == nil {
		return unknown
	}
	percent, err := strconv.Atoi(match[1])
	if err != nil {
		return unknown
	}
	total, err := strconv.Atoi(strings.ReplaceAll(match[2], ",", ""))
	if err != nil {
		return unknown
	}
	return NormalizeReview(label, &percent, &total)
}

// NormalizeReview accepts only an explicit known English aggregate summary.
// Missing evidence never means zero reviews. Unknown results carry no input
// label or numbers so downstream callers cannot publish malformed fragments.
func NormalizeReview(label string, percent *int, total *int) ReviewSummary {
	unknown := ReviewSummary{Band: ReviewUnknown}
	label, ok := normalizedSearchText(label, 64)
	if !ok || percent != nil && (*percent < 0 || *percent > 100) || total != nil && *total < 0 {
		return unknown
	}
	band := ReviewUnknown
	switch label {
	case "Overwhelmingly Positive", "Very Positive", "Positive", "Mostly Positive":
		band = ReviewPositive
	case "Mixed", "Mostly Negative", "Negative", "Very Negative", "Overwhelmingly Negative":
		band = ReviewMixedOrBelow
	case "No user reviews":
		if percent == nil && (total == nil || *total == 0) {
			return ReviewSummary{Band: ReviewUnrated, Label: label}
		}
		return unknown
	default:
		return unknown
	}
	if total == nil || *total == 0 || percent == nil || band == ReviewPositive && *percent < 70 || band == ReviewMixedOrBelow && *percent >= 70 {
		return unknown
	}
	value := *percent
	return ReviewSummary{Band: band, Label: label, Total: int64(*total), PositivePercent: &value}
}

func normalizedSearchText(raw string, limit int) (string, bool) {
	if !utf8.ValidString(raw) {
		return "", false
	}
	for _, r := range raw {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return "", false
		}
	}
	text := strings.Join(strings.Fields(raw), " ")
	return text, utf8.RuneCountInString(text) <= limit
}

func searchPageInteger(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	text := string(bytes.TrimSpace(raw))
	if strings.HasPrefix(text, `"`) {
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
	}
	value, err := strconv.Atoi(text)
	return value, err == nil && value >= 0 && strconv.Itoa(value) == text
}
