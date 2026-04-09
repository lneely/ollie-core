all:V: install

install:V:
	mkdir -p $HOME/.config/ollie
	cp -rf agents $HOME/.config/ollie

