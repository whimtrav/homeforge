package api

// App distribution — HomeForge hosts its own Android APK + a version manifest so
// the app can self-update without the Play Store. The APK download is GATED behind
// an HF account: /download is a login-then-download page, and the APK itself is
// only served to a valid hf_session (a stranger with the URL is bounced to login).
// The version manifest stays public (it's just a number the app polls to decide
// whether to prompt for an update).
//   First install: open /download in a browser → log in with the HF account → tap
//   Download. Updates: the app opens /download (already logged in → one more tap).

import (
	"net/http"
	"os"
)

const (
	appApkPath      = "/data/homeforge.apk"
	appManifestPath = "/data/app-latest.json"
)

// GET /download/latest.json — {version_code, version_name, url, notes}. Public: a
// version number isn't sensitive, and the app polls it on launch to decide whether
// to prompt. The APK itself is gated, so this doesn't hand out the binary.
func (s *Server) handleAppManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if data, err := os.ReadFile(appManifestPath); err == nil {
		w.Write(data)
		return
	}
	w.Write([]byte(`{"version_code":0}`))
}

// GET /download/homeforge.apk — the APK. Requires a valid HF session; a request
// without one is redirected to the login page.
func (s *Server) handleAppDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionEmail(r); !ok {
		http.Redirect(w, r, "/download", http.StatusFound)
		return
	}
	if _, err := os.Stat(appApkPath); err != nil {
		http.Error(w, "no build available yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="homeforge.apk"`)
	http.ServeFile(w, r, appApkPath)
}

// GET /download — self-contained login→download page. Checks the session client-
// side (/api/auth/me); logged in → shows the download button; otherwise shows a
// login form that POSTs to /api/auth/login (the same endpoint the app uses) and
// reveals the button on success.
func (s *Server) handleDownloadPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(downloadPageHTML))
}

const downloadPageHTML = `<!doctype html><html><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1"><title>HomeForge</title>
<style>
 body{font-family:system-ui,-apple-system,sans-serif;background:#0f1117;color:#e2e8f0;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}
 .c{width:320px;max-width:88vw;padding:24px;text-align:center}
 h1{margin:0 0 2px}
 .sub{color:#64748b;margin:0 0 20px}
 input{width:100%;box-sizing:border-box;background:#1a1d27;border:1px solid #262a38;color:#e2e8f0;padding:12px;border-radius:10px;margin:6px 0;font-size:15px}
 button,a.btn{display:block;width:100%;box-sizing:border-box;background:#6366f1;color:#fff;padding:14px;border:0;border-radius:12px;text-decoration:none;font-weight:600;margin-top:12px;font-size:16px;cursor:pointer}
 .msg{color:#ef4444;font-size:13px;min-height:16px;margin-top:8px}
 .note{color:#64748b;font-size:13px;margin-top:20px;line-height:1.4}
 .hide{display:none}
 .logo{width:78px;height:78px;display:block;margin:0 auto 10px}
 h1{font-weight:800;letter-spacing:-.02em}
 .fg{color:#f5642a}
</style></head><body><div class=c>
 <svg class=logo viewBox="0 0 512 512" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><defs><filter id="lg" x="-40%" y="-40%" width="180%" height="180%"><feGaussianBlur stdDeviation="4.5" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter><linearGradient id="lcf" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#333B4C"/><stop offset="1" stop-color="#161A24"/></linearGradient></defs><g filter="url(#lg)"><g fill="none" stroke="#35C6FF" stroke-width="6" stroke-linecap="round" stroke-linejoin="round"><path d="M168 200 H64"/><path d="M168 256 H40"/><path d="M168 312 H74 L48 338"/><path d="M344 200 H448"/><path d="M344 256 H472"/><path d="M344 312 H438 L464 338"/><path d="M200 168 V64"/><path d="M256 168 V40"/><path d="M312 168 V74 L338 48"/><path d="M200 344 V448"/><path d="M256 344 V472"/><path d="M312 344 V438 L338 464"/></g><g fill="#35C6FF"><circle cx="58" cy="200" r="6"/><circle cx="34" cy="256" r="6"/><circle cx="42" cy="342" r="6"/><circle cx="454" cy="200" r="6"/><circle cx="478" cy="256" r="6"/><circle cx="470" cy="342" r="6"/><circle cx="200" cy="58" r="6"/><circle cx="256" cy="34" r="6"/><circle cx="342" cy="42" r="6"/><circle cx="200" cy="454" r="6"/><circle cx="256" cy="478" r="6"/><circle cx="342" cy="470" r="6"/></g></g><rect x="168" y="168" width="176" height="176" rx="30" fill="url(#lcf)"/><g filter="url(#lg)"><rect x="168" y="168" width="176" height="176" rx="30" fill="none" stroke="#35C6FF" stroke-width="9"/><path d="M256 204 L298 250 L288 250 L288 314 L224 314 L224 250 L214 250 Z" fill="#35C6FF"/><rect x="244" y="286" width="24" height="28" fill="#F5642A"/></g></svg>
 <h1>Home<span class=fg>Forge</span></h1>
 <p class=sub id=ver>&nbsp;</p>
 <div id=login class=hide>
   <input id=email type=email placeholder="Email" autocomplete=username>
   <input id=pw type=password placeholder="Password" autocomplete=current-password>
   <button id=go>Log in</button>
   <div class=msg id=msg></div>
 </div>
 <div id=dl class=hide>
   <a class=btn href="/download/homeforge.apk">Download &amp; install</a>
   <p class=note>After it downloads, tap the file. Android will ask to allow installs from your browser &mdash; say yes.</p>
 </div>
</div>
<script>
 var login=document.getElementById('login'),dl=document.getElementById('dl'),msg=document.getElementById('msg');
 function show(authed){ login.className=authed?'hide':''; dl.className=authed?'':'hide'; }
 fetch('/download/latest.json').then(function(r){return r.json();}).then(function(d){ if(d.version_name){document.getElementById('ver').textContent='Version '+d.version_name;} }).catch(function(){});
 fetch('/api/auth/me',{credentials:'same-origin'}).then(function(r){return r.json();}).then(function(d){ show(d.authenticated===true); }).catch(function(){ show(false); });
 document.getElementById('go').onclick=function(){
   msg.textContent='';
   var b={email:document.getElementById('email').value,password:document.getElementById('pw').value};
   fetch('/api/auth/login',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)})
     .then(function(r){ if(r.status===200){ show(true); } else { msg.textContent='Wrong email or password'; } })
     .catch(function(){ msg.textContent='Cannot reach server'; });
 };
 document.getElementById('pw').addEventListener('keydown',function(e){ if(e.key==='Enter'){ document.getElementById('go').click(); } });
</script></body></html>`
