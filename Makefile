include .env

clean:
	rm -rf output cache

build:
	docker build -t tinfoil-modelwrap .

pack:
	docker run --rm -it \
		-v $(shell pwd)/cache:/cache \
		-v /opt/tinfoil/hfmodels:/output \
		-e HF_TOKEN=${HF_TOKEN} \
		-e MODEL=$(word 2,$(MAKECMDGOALS)) \
		tinfoil-modelwrap

%:
	@:
