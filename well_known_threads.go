package awfulirc

// WellKnownThreads contains a manual listing of threads with canonical names
// that will be reserved before additional bookmarks are processed.
//
// The Hint can be any string, and does not even need to be unique, but is most
// useful if it is a short unique string that will form the channel name.
var WellKnownThreads = []ThreadMetadata{
	{
		ID:    4102933,
		Title: "awfulirc help and discussion",
		Hint:  "help",
	},
	{
		ID:    4055683,
		Title: "Legends - PYF SA Legends",
		Hint:  "legends",
	},
	{
		ID:    4083561,
		Title: "[L@@K] The thread where you can see when threads are opened in SAD",
		Hint:  "sad",
	},
}
