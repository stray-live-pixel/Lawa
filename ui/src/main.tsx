import { ToasterProvider, ToasterComponent } from '@gravity-ui/uikit';
import { toaster } from '@gravity-ui/uikit/toaster-singleton';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import '@gravity-ui/uikit/styles/fonts.css';
import '@gravity-ui/uikit/styles/styles.css';
import './styles.css';
import { AppTheme } from './components/Theme';

// Единственная точка запуска: страницы и граф используют общее дерево React.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <AppTheme>
        <ToasterProvider toaster={toaster}>
          <App />
          <ToasterComponent />
        </ToasterProvider>
      </AppTheme>
    </BrowserRouter>
  </StrictMode>,
);
