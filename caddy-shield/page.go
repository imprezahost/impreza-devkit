package caddyshield

import (
	"encoding/json"
	"strings"
)

// renderChallengePage produces the fully self-contained PoW challenge page.
// No external assets, no third-party CAPTCHA, inline styles and an inline
// pure-JavaScript SHA-256 (works on plain-HTTP onion origins where the Web
// Crypto API is not guaranteed). Values are injected as JSON string literals
// with HTML escaping, so nothing from the request reaches the page raw.
func renderChallengePage(challenge string, difficulty int, next string) string {
	chJSON, _ := json.Marshal(challenge)
	nextJSON, _ := json.Marshal(next)
	diffJSON, _ := json.Marshal(difficulty)
	verifyJSON, _ := json.Marshal(VerifyPath)
	page := strings.NewReplacer(
		"__CHALLENGE__", string(chJSON),
		"__DIFFICULTY__", string(diffJSON),
		"__NEXT__", string(nextJSON),
		"__VERIFY__", string(verifyJSON),
	).Replace(challengePageTemplate)
	if strings.Contains(page, "__CHALLENGE__") {
		panic("caddy-shield: challenge template placeholder not replaced")
	}
	return page
}

const challengePageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Security check</title>
<style>
:root{color-scheme:light dark}
body{font-family:system-ui,sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#f5f5f5;color:#222}
@media (prefers-color-scheme:dark){body{background:#1c1c1e;color:#eee}}
main{max-width:28rem;padding:2rem;margin:1rem;background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.15);text-align:center}
@media (prefers-color-scheme:dark){main{background:#2c2c2e}}
h1{font-size:1.2rem;margin:0 0 .5rem}
p{font-size:.95rem;line-height:1.5}
.bar{height:8px;border-radius:4px;background:#ddd;overflow:hidden;margin:1rem 0}
@media (prefers-color-scheme:dark){.bar{background:#3a3a3c}}
.bar>div{height:100%;width:0;background:#0a7;height-transition:width .3s}
#s{font-size:.85rem;opacity:.75;min-height:1.2em}
</style>
</head>
<body>
<main>
<h1>Verifying your browser&hellip;</h1>
<p>This site is protected by a lightweight proof-of-work check against automated abuse. Solving it usually takes a moment.</p>
<noscript><p><strong>JavaScript is required</strong> to pass this check. Enable it and reload.</p></noscript>
<div class="bar"><div id="pb"></div></div>
<p id="s">Working&hellip;</p>
</main>
<script>
(function(){
"use strict";
var CH=__CHALLENGE__, DIFF=__DIFFICULTY__, NEXT=__NEXT__, VERIFY=__VERIFY__;

// Pure-JS SHA-256 (FIPS 180-4) over an ASCII string, returning lowercase hex.
var K=[0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2];
function rr(x,n){return (x>>>n)|(x<<(32-n));}
function sha256hex(s){
 var m=[],i;
 for(i=0;i<s.length;i++){m[i>>2]=(m[i>>2]||0)|((s.charCodeAt(i)&0xff)<<(24-(i%4)*8));}
 var bl=s.length*8;
 m[bl>>5]|=0x80<<(24-bl%32);
 m[((bl+64>>9)<<4)+15]=bl;
 for(i=m.length-1;i>0;i--){if(m[i]===undefined)m[i]=0;}
 var H=[0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19];
 var w=new Array(64);
 for(i=0;i<m.length;i+=16){
  var j,a,b,c,d,e,f,g,h,t1,t2;
  for(j=0;j<16;j++){w[j]=m[i+j]||0;}
  for(j=16;j<64;j++){
   var s0=rr(w[j-15],7)^rr(w[j-15],18)^(w[j-15]>>>3);
   var s1=rr(w[j-2],17)^rr(w[j-2],19)^(w[j-2]>>>10);
   w[j]=(w[j-16]+s0+w[j-7]+s1)|0;
  }
  a=H[0];b=H[1];c=H[2];d=H[3];e=H[4];f=H[5];g=H[6];h=H[7];
  for(j=0;j<64;j++){
   var S1=rr(e,6)^rr(e,11)^rr(e,25);
   var ch=(e&f)^(~e&g);
   t1=(h+S1+ch+K[j]+w[j])|0;
   var S0=rr(a,2)^rr(a,13)^rr(a,22);
   var mj=(a&b)^(a&c)^(b&c);
   t2=(S0+mj)|0;
   h=g;g=f;f=e;e=(d+t1)|0;d=c;c=b;b=a;a=(t1+t2)|0;
  }
  H[0]=(H[0]+a)|0;H[1]=(H[1]+b)|0;H[2]=(H[2]+c)|0;H[3]=(H[3]+d)|0;
  H[4]=(H[4]+e)|0;H[5]=(H[5]+f)|0;H[6]=(H[6]+g)|0;H[7]=(H[7]+h)|0;
 }
 var out="";
 for(i=0;i<8;i++){out+=(H[i]>>>0).toString(16).padStart(8,"0");}
 return out;
}
var target="",k;
for(k=0;k<DIFF;k++){target+="0";}
var expect=Math.pow(16,DIFF);
var nonce=0,found=-1;
var pb=document.getElementById("pb"),st=document.getElementById("s");
function step(){
 var t0=Date.now();
 while(Date.now()-t0<60){
  if(sha256hex(CH+":"+nonce).substring(0,DIFF)===target){found=nonce;break;}
  nonce++;
 }
 if(found>=0){
  pb.style.width="100%";
  st.textContent="Solved. Verifying\u2026";
  fetch(VERIFY,{method:"POST",headers:{"content-type":"application/x-www-form-urlencoded"},
   body:"challenge="+encodeURIComponent(CH)+"&nonce="+found+"&next="+encodeURIComponent(NEXT),
   redirect:"manual"}).then(function(r){
    if(r.status>=200&&r.status<400){location.replace(NEXT);}
    else{st.textContent="Verification failed. Reload the page to retry.";}
   }).catch(function(){st.textContent="Network error. Reload the page to retry.";});
  return;
 }
 pb.style.width=Math.min(95,Math.round(100*nonce/expect))+"%";
 setTimeout(step,0);
}
step();
})();
</script>
</body>
</html>`
