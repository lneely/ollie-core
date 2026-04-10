all:V: install

install:V:
	mkdir -p $HOME/.config/ollie/sandbox
	mkdir -p $HOME/.config/ollie/prompts
	cp -rf agents $HOME/.config/ollie
	cp -rf sandbox $HOME/.config/ollie
	cp -rf prompts $HOME/.config/ollie

