all:V: install

install:V:
	mkdir -p $HOME/.config/ollie/sandbox
	mkdir -p $HOME/.config/ollie/prompts
	mkdir -p $HOME/.config/ollie/skills
	cp -rf agents $HOME/.config/ollie
	cp -rf sandbox $HOME/.config/ollie
	cp -rf prompts $HOME/.config/ollie

