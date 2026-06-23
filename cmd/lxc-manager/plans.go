package main

// PlanInfo is the public shape returned by GET /api/plans.
type PlanInfo struct {
	ID          string `json:"id"`
	CPU         int    `json:"cpu"`
	Mem         int    `json:"mem"`
	Disk        int    `json:"disk"`
	MonthlyCost int    `json:"monthlyCost"`
	Bandwidth   int    `json:"bandwidth"`
}

// plans is the static list of available container specifications.
// Prices are in US cents/month. Bandwidth is in GB/month.
// This list is curated by the platform operator; the LXC API does not
// query nodes dynamically for available plans.
var plans = []PlanInfo{
	{ID: "lxc-1c-512mb", CPU: 1, Mem: 512, Disk: 10, MonthlyCost: 300, Bandwidth: 512},
	{ID: "lxc-1c-1gb", CPU: 1, Mem: 1024, Disk: 25, MonthlyCost: 600, Bandwidth: 1024},
	{ID: "lxc-1c-2gb", CPU: 1, Mem: 2048, Disk: 50, MonthlyCost: 1200, Bandwidth: 2048},
	{ID: "lxc-2c-2gb", CPU: 2, Mem: 2048, Disk: 65, MonthlyCost: 1800, Bandwidth: 3072},
	{ID: "lxc-2c-4gb", CPU: 2, Mem: 4096, Disk: 80, MonthlyCost: 2400, Bandwidth: 3072},
}
