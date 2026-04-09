all:V: install

install:V:
	mkdir -p $HOME/.config/ollie/sandbox
	cp -rf agents $HOME/.config/ollie
	cp -rf sandbox $HOME/.config/ollie

