package brand

// PigeonArt is the mascot, facing left. It is the single source for both the
// terminal splash (internal/tui/splash.go) and the web splash (injected into
// index.html by the server). Keep it ≤ 18 columns wide and ≤ 9 rows tall so
// it sits inside the hexagon.
var PigeonArt = []string{
	`     __`,
	`   /' o)`,
	`  <    (`,
	`   \    '-.__`,
	`   /  ((((   '-.`,
	`  |   ((((( _.-'`,
	`   \___.--''`,
	`      ||`,
	`     _/\_`,
}

// PigeonMask colours PigeonArt cell by cell (same shape; space = body).
//
//	e eye   k beak   n iridescent neck   w wing   f feet
var PigeonMask = []string{
	`     nn`,
	`   nn e `,
	`  k    n`,
	`   n    nn  `,
	`      wwww     `,
	`      wwwww    `,
	`            `,
	`      ff`,
	`     ffff`,
}
