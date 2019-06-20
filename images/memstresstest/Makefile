CXX ?= g++
everything: mem
clean:
	rm -f `cat .gitignore`
rebuild: clean mem
mem:
	$(CXX) mem.cc -o mem
test: mem
	./mem || true
