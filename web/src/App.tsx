import { useEffect, useState } from 'react';
import {
  BrowserRouter,
  NavLink,
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from 'react-router';
import {
  Activity,
  GitBranch,
  Home,
  Layers,
  LogOut,
  Menu,
  Monitor,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Plus,
  Settings,
  Sun,
} from 'lucide-react';
import { api, APIError } from './api';
import { FerryMark, StatusBadge } from './components/UI';
import { I18nProvider, useI18n } from './i18n';
import { ActivityPage } from './pages/ActivityPage';
import { HomePage } from './pages/HomePage';
import { ServicesPage } from './pages/ServicesPage';
import { SettingsPage } from './pages/SettingsPage';
import { SourcesPage } from './pages/SourcesPage';
import { ThemeProvider, useTheme, type ThemeMode } from './theme';
import type { MeResponse } from './types';

const SIDEBAR_KEY = 'ferry.sidebarCollapsed';

export function App() {
  return (
    <ThemeProvider>
      <I18nProvider>
        <BrowserRouter>
          <Shell />
        </BrowserRouter>
      </I18nProvider>
    </ThemeProvider>
  );
}

function Shell() {
  const { t, locale, setLocale } = useI18n();
  const { mode, setMode } = useTheme();
  const navigate = useNavigate();
  const location = useLocation();

  const [me, setMe] = useState<MeResponse | null>(null);
  const [unauthed, setUnauthed] = useState(false);
  const [loading, setLoading] = useState(true);

  const [collapsed, setCollapsed] = useState<boolean>(() => {
    return typeof window !== 'undefined' && localStorage.getItem(SIDEBAR_KEY) === '1';
  });
  const [mobileOpen, setMobileOpen] = useState(false);
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const [focusTrigger, setFocusTrigger] = useState(0);

  useEffect(() => {
    api
      .me()
      .then((res) => {
        setMe(res);
        setUnauthed(false);
      })
      .catch((err) => {
        if (err instanceof APIError && err.status === 401) {
          setUnauthed(true);
        }
      })
      .finally(() => setLoading(false));
  }, []);

  // Close mobile drawer and user menu on navigation
  useEffect(() => {
    setMobileOpen(false);
    setUserMenuOpen(false);
  }, [location.pathname]);

  useEffect(() => {
    const closeMenu = () => setUserMenuOpen(false);
    window.addEventListener('click', closeMenu);
    return () => window.removeEventListener('click', closeMenu);
  }, []);

  // Global `/` shortcut to focus Home deploy composer
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setUserMenuOpen(false);
        setMobileOpen(false);
        return;
      }
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
      const el = e.target as HTMLElement | null;
      if (
        el &&
        (el.tagName === 'INPUT' ||
          el.tagName === 'TEXTAREA' ||
          el.tagName === 'SELECT' ||
          el.isContentEditable)
      ) {
        return;
      }
      e.preventDefault();
      if (location.pathname !== '/') {
        navigate('/');
      }
      setFocusTrigger((n) => n + 1);
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [location.pathname, navigate]);

  const toggleCollapsed = () => {
    setCollapsed((prev) => {
      const next = !prev;
      localStorage.setItem(SIDEBAR_KEY, next ? '1' : '0');
      return next;
    });
  };

  const handleLogout = async () => {
    await api.logout().catch(() => {});
    window.location.href = '/';
  };

  if (loading) {
    return <div className="boot-screen" />;
  }

  if (unauthed || !me) {
    const nextUrl = `/auth/login?next=${encodeURIComponent(location.pathname + location.search)}`;
    return (
      <div className="signin-screen">
        <div className="surface-card signin-card">
          <FerryMark size={40} />
          <h1 className="serif-heading signin-title">{t.signInTitle}</h1>
          <p className="text-sm text-muted signin-sub">{t.signInSubtitle}</p>
          <a href={nextUrl} className="btn btn-primary signin-btn">
            {t.signInButton}
          </a>
        </div>
      </div>
    );
  }

  const navItems = [
    { to: '/', label: t.navHome, icon: Home, end: true },
    { to: '/services', label: t.navServices, icon: Layers },
    { to: '/sources', label: t.navSources, icon: GitBranch },
    { to: '/activity', label: t.navActivity, icon: Activity },
    { to: '/settings', label: t.navSettings, icon: Settings },
  ];

  const initials = (me.name || me.email || 'U')
    .trim()
    .slice(0, 1)
    .toUpperCase();

  return (
    <div className={`app-shell ${collapsed ? 'is-collapsed' : ''}`}>
      {/* Mobile top bar */}
      <div className="mobile-topbar">
        <button
          type="button"
          className="icon-btn"
          onClick={() => setMobileOpen(true)}
          aria-label="Open menu"
        >
          <Menu size={20} />
        </button>
        <div className="row-gap-xs">
          <FerryMark size={24} />
          <span className="brand-wordmark">Ferry</span>
        </div>
        <div style={{ width: 32 }} />
      </div>

      {mobileOpen && (
        <div
          className="mobile-backdrop"
          onClick={() => setMobileOpen(false)}
          role="presentation"
        />
      )}

      <aside className={`sidebar ${mobileOpen ? 'is-mobile-open' : ''}`}>
        <div className="sidebar-top">
          <NavLink to="/" className="brand-link" title="Ferry">
            <FerryMark size={26} />
            {!collapsed && <span className="brand-wordmark">Ferry</span>}
          </NavLink>
          <button
            type="button"
            className="icon-btn collapse-btn"
            onClick={toggleCollapsed}
            title={collapsed ? t.expandSidebar : t.collapseSidebar}
          >
            {collapsed ? (
              <PanelLeftOpen size={17} strokeWidth={1.7} />
            ) : (
              <PanelLeftClose size={17} strokeWidth={1.7} />
            )}
          </button>
        </div>

        <div className="sidebar-action">
          <button
            type="button"
            className="new-deploy-btn"
            onClick={() => {
              navigate('/');
              setFocusTrigger((n) => n + 1);
            }}
            title={t.newDeploy}
          >
            <span className="new-deploy-icon">
              <Plus size={15} strokeWidth={2.2} />
            </span>
            {!collapsed && (
              <>
                <span className="new-deploy-label">{t.newDeploy}</span>
                <kbd className="kbd-hint">/</kbd>
              </>
            )}
          </button>
        </div>

        <nav className="sidebar-nav">
          {navItems.map((item) => {
            const Icon = item.icon;
            return (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) =>
                  `nav-item ${isActive ? 'is-active' : ''}`
                }
                title={collapsed ? item.label : undefined}
              >
                <Icon size={18} strokeWidth={1.7} className="shrink-0" />
                {!collapsed && <span>{item.label}</span>}
              </NavLink>
            );
          })}
        </nav>

        <div
          className="sidebar-bottom"
          onClick={(e) => e.stopPropagation()}
        >
          {userMenuOpen && (
            <div className="user-popover">
              <div className="popover-section">
                <div className="popover-label">{t.themeLabel}</div>
                <div className="segmented-row">
                  {(
                    [
                      { id: 'light', label: t.themeLight, icon: Sun },
                      { id: 'dark', label: t.themeDark, icon: Moon },
                      { id: 'system', label: t.themeSystem, icon: Monitor },
                    ] as Array<{ id: ThemeMode; label: string; icon: typeof Sun }>
                  ).map((opt) => {
                    const Icon = opt.icon;
                    return (
                      <button
                        key={opt.id}
                        type="button"
                        className={`seg-btn ${mode === opt.id ? 'active' : ''}`}
                        onClick={() => setMode(opt.id)}
                      >
                        <Icon size={13} />
                        <span>{opt.label}</span>
                      </button>
                    );
                  })}
                </div>
              </div>

              <div className="popover-section">
                <div className="popover-label">{t.languageLabel}</div>
                <div className="segmented-row">
                  <button
                    type="button"
                    className={`seg-btn ${locale === 'en' ? 'active' : ''}`}
                    onClick={() => setLocale('en')}
                  >
                    English
                  </button>
                  <button
                    type="button"
                    className={`seg-btn ${locale === 'zh-CN' ? 'active' : ''}`}
                    onClick={() => setLocale('zh-CN')}
                  >
                    简体中文
                  </button>
                </div>
              </div>

              {!me.authDisabled && (
                <button
                  type="button"
                  className="popover-logout"
                  onClick={handleLogout}
                >
                  <LogOut size={14} />
                  <span>{t.signOut}</span>
                </button>
              )}
            </div>
          )}

          <button
            type="button"
            className="user-pill"
            onClick={() => setUserMenuOpen((v) => !v)}
            title={me.email || me.name}
          >
            <span className="user-avatar">{initials}</span>
            {!collapsed && (
              <div className="user-info min-w-0">
                <div className="row-gap-xs">
                  <span className="user-name truncate">{me.name}</span>
                  {me.authDisabled && (
                    <StatusBadge tone="neutral">{t.demoBadge}</StatusBadge>
                  )}
                </div>
                {me.email && (
                  <span className="user-email truncate">{me.email}</span>
                )}
              </div>
            )}
          </button>
        </div>
      </aside>

      <main className="main-content">
        <Routes>
          <Route
            path="/"
            element={<HomePage me={me} focusTrigger={focusTrigger} />}
          />
          <Route path="/services" element={<ServicesPage />} />
          <Route path="/sources" element={<SourcesPage me={me} />} />
          <Route path="/activity" element={<ActivityPage />} />
          <Route path="/settings" element={<SettingsPage me={me} />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}
