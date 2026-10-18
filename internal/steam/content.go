package steam

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// NormalizeGameContent gives catalog identities and AI evidence the same stable
// ordering and whitespace without mutating the caller's metadata slices.
func NormalizeGameContent(details GameDetails) GameDetails {
	details.Name = strings.Join(strings.Fields(details.Name), " ")
	details.ShortDescription = strings.Join(strings.Fields(details.ShortDescription), " ")
	details.Genres = append([]Genre(nil), details.Genres...)
	sort.Slice(details.Genres, func(i, j int) bool {
		if details.Genres[i].ID != details.Genres[j].ID {
			return details.Genres[i].ID < details.Genres[j].ID
		}
		return details.Genres[i].Description < details.Genres[j].Description
	})
	details.Categories = append([]Category(nil), details.Categories...)
	sort.Slice(details.Categories, func(i, j int) bool {
		if details.Categories[i].ID != details.Categories[j].ID {
			return details.Categories[i].ID < details.Categories[j].ID
		}
		return details.Categories[i].Description < details.Categories[j].Description
	})
	details.Developers = append([]string(nil), details.Developers...)
	sort.Strings(details.Developers)
	details.Publishers = append([]string(nil), details.Publishers...)
	sort.Strings(details.Publishers)
	return details
}

// GameContentHash identifies semantic metadata while ignoring presentation,
// availability, and release changes that do not require new AI extraction.
func GameContentHash(details GameDetails) string {
	details = NormalizeGameContent(details)
	evidence := struct {
		Name, Description      string
		Genres                 []Genre
		Categories             []Category
		Developers, Publishers []string
	}{details.Name, details.ShortDescription, details.Genres, details.Categories, details.Developers, details.Publishers}
	raw, _ := json.Marshal(evidence)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
