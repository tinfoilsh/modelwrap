# Modelwrap

Builds reproducible dm-verity EROFS images of Hugging Face models.

## Pack image

```bash
docker run --rm -it \
		-v $(shell pwd)/cache:/cache \
		-v $(shell pwd)/output:/output \
		-e HF_TOKEN=${HF_TOKEN} \
		-e MODEL=meta-llama/Llama-3.2-1B@4e20de362430cd3b72f300e6b0f18e50e7166e08 \
		ghcr.io/tinfoilsh/modelwrap
```

You may ommit the `@revision` suffix to instruct modelwrap to retrieve the latest commit of the first branch.

`modelwrap` emits two files in the output directory:

- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.mpk`: dm-verity EROFS image
- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.info`: metadata file in the format `ROOTHASH_OFFSET_VERITYUUID`
