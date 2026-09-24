FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends python3 curl ca-certificates util-linux && rm -rf /var/lib/apt/lists/*
COPY internal/bootstrap/recipe.sh /test/recipe.sh
COPY internal/bootstrap/testdata/smoke.py /test/smoke.py
RUN touch /run/dashnet-recipe-test-container
CMD ["python3", "/test/smoke.py"]
