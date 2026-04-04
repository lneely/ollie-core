INSTALL_PATH=$HOME/bin

all:V: install

build:V:
	go build -o $INSTALL_PATH/ollie
	mkdir -p $HOME/.config/ollie
	cp -rf agents $HOME/.config/ollie

install:V: build

clean:V:
	rm -f $INSTALL_PATH/ollie

