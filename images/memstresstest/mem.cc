#include <iostream>
#include <unistd.h>
#include <stdlib.h>
#include <stdio.h>
#include <stdint.h>
#include <string.h>
#include <time.h>

using namespace std;

const size_t MB10 = 1024 * 1024 * 10;

int main (int argc, char** argv) {
  size_t i = 0;
  while (true) {
    cout << "Allocating " << (i * 10) << "MB of memory..." << endl;
    void* vl = malloc(MB10);
    if (vl == NULL) {
      cout << endl;
      cerr << "Out of memory :(" << endl;
      return 1;
    }
    memset(vl, 0xab, MB10);
    i++;
  }
}
