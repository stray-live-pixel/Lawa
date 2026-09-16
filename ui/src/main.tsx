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
        <App />
      </AppTheme>
    </BrowserRouter>
  </StrictMode>,
);
