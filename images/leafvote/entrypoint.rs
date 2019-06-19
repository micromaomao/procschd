use std;
use std::io::{Read, Write};
use std::string::String;
use std::mem;
use std::process::Command;
use std::os::unix::process::CommandExt;

fn exit<E: std::fmt::Display>(err: E) -> ! {
  let _ = std::io::stderr().write_fmt(format_args!("Error: {}", err));
  std::process::exit(1)
}

fn main () {
  let stdin = std::io::stdin();
  let mut stdin = stdin.lock();
  let mut inbuf = Vec::new();
  let res = stdin.read_to_end(&mut inbuf);
  if let Err(e) = res {
    exit(e);
  }
  let instr = String::from_utf8(inbuf);
  if let Err(e) = instr {
    exit(e);
  }
  let instr = instr.unwrap();
  let tokens = instr.split('\n');
  let template = include_str!("voteticket.tex");
  let mut templateParts = template.split("%%%%%%% PLACEHOLDER %%%%%%%");
  let templateStart = templateParts.next().unwrap();
  let templateEnd = templateParts.next().unwrap();
  let mut latexFile = std::fs::File::create("./in.tex");
  if let Err(e) = latexFile {
    exit(e);
  }
  assert!(templateParts.next().is_none());
  let mut latexFile = latexFile.unwrap();
  if let Err(e) = latexFile.write_all(templateStart.as_bytes()) {
    exit(e);
  }
  const iX: i32 = 4;
  const iY: i32 = 264;
  let mut cX = iX;
  let mut cY = iY;
  const xInc: i32 = 82;
  const yInc: i32 = 31;
  const pageW: i32 = 210;
  const pageH: i32 = 297;
  let mut currentPageLatex = std::cell::RefCell::new(String::new());
  let mut writeCurrentPage = || {
    let currentPageLatex = currentPageLatex.borrow();
    let currentPageLatex = currentPageLatex.trim();
    if currentPageLatex.len() > 0 {
      let currentPageLatex = currentPageLatex.replace("\n", "\n    ");
      latexFile.write_all("  \\pg{\n    ".as_bytes()).unwrap();
      latexFile.write_all(currentPageLatex.as_bytes()).unwrap();
      latexFile.write_all("\n  }%\n".as_bytes()).unwrap();
    }
  };
  for token in tokens {
    let tokenUrlEncodeded = token; // TODO
    currentPageLatex.borrow_mut().push_str(&format!("\\begin{{scope}}[shift={{({}mm,{}mm)}}]\n  \
                                      \\slitcontent{{{}}}{{{}}}\n\\end{{scope}}\n", cX, cY, token, tokenUrlEncodeded));
    if cX + xInc*2 < pageW {
      cX += xInc;
    } else {
      cX = iX;
      if cY - yInc > 0 {
        cY -= yInc;
      } else {
        cY = iY;
        writeCurrentPage();
        currentPageLatex.borrow_mut().clear();
      }
    }
  }
  writeCurrentPage();
  mem::drop(writeCurrentPage);
  latexFile.write_all(templateEnd.as_bytes()).unwrap();
  mem::drop(latexFile);
  let res = Command::new("latex").arg("in.tex").status();
  if let Err(e) = res {
    exit(e);
  }
  let res = res.unwrap();
  if !res.success() {
    exit(format!("latex failed with code {}", res.code().unwrap_or(-1)));
  }
  let res = Command::new("dvipdf").arg("in.dvi").status();
  if let Err(e) = res {
    exit(e);
  }
  let res = res.unwrap();
  if !res.success() {
    exit(format!("dvipdf failed with code {}", res.code().unwrap_or(-1)));
  }
}
