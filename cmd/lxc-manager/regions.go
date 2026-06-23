package main

// regionMeta maps region IDs to their display metadata.
// Region IDs are arbitrary short strings configured in nodes.json.
// Add new regions here as nodes are deployed to new locations.
//
// Country codes follow ISO 3166-1 alpha-2:
// https://en.wikipedia.org/wiki/ISO_3166-1_alpha-2
var regionMeta = map[string]struct {
	City    string `json:"city"`
	Country string `json:"country"`
}{
	"tokyo":       {City: "Tokyo", Country: "JP"},
	"ewr":         {City: "Newark", Country: "US"},
	"lax":         {City: "Los Angeles", Country: "US"},
	"sfo":         {City: "San Francisco", Country: "US"},
	"sea":         {City: "Seattle", Country: "US"},
	"dal":         {City: "Dallas", Country: "US"},
	"chi":         {City: "Chicago", Country: "US"},
	"mia":         {City: "Miami", Country: "US"},
	"lon":         {City: "London", Country: "GB"},
	"ams":         {City: "Amsterdam", Country: "NL"},
	"fra":         {City: "Frankfurt", Country: "DE"},
	"par":         {City: "Paris", Country: "FR"},
	"sgp":         {City: "Singapore", Country: "SG"},
	"syd":         {City: "Sydney", Country: "AU"},
	"tor":         {City: "Toronto", Country: "CA"},
	"sao":         {City: "São Paulo", Country: "BR"},
	"bom":         {City: "Mumbai", Country: "IN"},
	"blr":         {City: "Bangalore", Country: "IN"},
	"icn":         {City: "Seoul", Country: "KR"},
	"nrt":         {City: "Tokyo (Narita)", Country: "JP"},
}

// RegionInfo is the public shape returned by GET /api/regions.
type RegionInfo struct {
	ID      string `json:"id"`
	City    string `json:"city"`
	Country string `json:"country"`
}
