.PHONY: manual pages-build

manual:
	go run ./cmd/srv-manual docs manual.html

pages-build:
	mkdir -p dist
	go run ./cmd/srv-manual docs dist/index.html
	touch dist/.nojekyll
