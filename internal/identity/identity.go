package identity

type Display struct {
	Name, CLI, Tagline, Description string
}

func Default() Display {
	return Display{
		Name: "LogP", CLI: "logp",
		Tagline:     "Every browser is a potential compute node.",
		Description: "Adaptive browser-based distributed computing",
	}
}
